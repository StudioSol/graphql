package graphql

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/StudioSol/graphql/gqlerrors"
	"github.com/StudioSol/graphql/language/ast"
)

type ExecuteParams struct {
	Schema        Schema
	Root          interface{}
	AST           *ast.Document
	OperationName string
	Args          map[string]interface{}

	// Context may be provided to pass application-specific per-request
	// information to resolve functions.
	Context context.Context

	PanicHandler PanicHandler
}

func Execute(p ExecuteParams) (result *Result) {
	// Use background context if no context was provided
	ctx := p.Context
	if ctx == nil {
		ctx = context.Background()
	}

	resultChannel := make(chan *Result)

	go func(out chan<- *Result, done <-chan struct{}) {
		result := &Result{}

		exeContext, err := buildExecutionContext(buildExecutionCtxParams{
			Schema:        p.Schema,
			Root:          p.Root,
			AST:           p.AST,
			OperationName: p.OperationName,
			Args:          p.Args,
			Errors:        nil,
			Result:        result,
			Context:       p.Context,
			PanicHandler:  p.PanicHandler,
		})

		if err != nil {
			result.Errors = append(result.Errors, gqlerrors.FormatError(err))
			select {
			case out <- result:
			case <-done:
			}
			return
		}

		defer func() {
			if r := recover(); r != nil {
				var err error
				if r, ok := r.(error); ok {
					err = gqlerrors.FormatError(r)
				}
				exeContext.Errors = append(exeContext.Errors, gqlerrors.FormatError(err))
				result.Errors = exeContext.Errors
				select {
				case out <- result:
				case <-done:
				}
			}
		}()

		result = executeOperation(executeOperationParams{
			ExecutionContext: exeContext,
			Root:             p.Root,
			Operation:        exeContext.Operation,
		})
		select {
		case out <- result:
		case <-done:
		}

	}(resultChannel, ctx.Done())

	select {
	case <-ctx.Done():
		result = &Result{}
		result.Errors = append(result.Errors, gqlerrors.FormatError(ctx.Err()))
	case r := <-resultChannel:
		result = r
	}
	return
}

type buildExecutionCtxParams struct {
	Schema        Schema
	Root          interface{}
	AST           *ast.Document
	OperationName string
	Args          map[string]interface{}
	Errors        []gqlerrors.FormattedError
	Result        *Result
	Context       context.Context
	PanicHandler  func(ctx context.Context, err interface{})
}

type executionContext struct {
	Schema         Schema
	Fragments      map[string]ast.Definition
	Root           interface{}
	Operation      ast.Definition
	VariableValues map[string]interface{}
	Errors         []gqlerrors.FormattedError
	Context        context.Context
	PanicHandler   func(ctx context.Context, err interface{})
}

func buildExecutionContext(p buildExecutionCtxParams) (*executionContext, error) {
	eCtx := &executionContext{}
	var operation *ast.OperationDefinition
	fragments := map[string]ast.Definition{}

	for _, definition := range p.AST.Definitions {
		switch definition := definition.(type) {
		case *ast.OperationDefinition:
			if (p.OperationName == "") && operation != nil {
				return nil, errors.New("Must provide operation name if query contains multiple operations.")
			}
			if p.OperationName == "" || definition.GetName() != nil && definition.GetName().Value == p.OperationName {
				operation = definition
			}
		case *ast.FragmentDefinition:
			key := ""
			if definition.GetName() != nil && definition.GetName().Value != "" {
				key = definition.GetName().Value
			}
			fragments[key] = definition
		default:
			return nil, fmt.Errorf("GraphQL cannot execute a request containing a %v", definition.GetKind())
		}
	}

	if operation == nil {
		if p.OperationName != "" {
			return nil, fmt.Errorf(`Unknown operation named "%v".`, p.OperationName)
		}
		return nil, fmt.Errorf(`Must provide an operation.`)
	}

	variableValues, err := getVariableValues(p.Schema, operation.GetVariableDefinitions(), p.Args)
	if err != nil {
		return nil, err
	}

	eCtx.Schema = p.Schema
	eCtx.Fragments = fragments
	eCtx.Root = p.Root
	eCtx.Operation = operation
	eCtx.VariableValues = variableValues
	eCtx.Errors = p.Errors
	eCtx.Context = p.Context
	eCtx.PanicHandler = p.PanicHandler
	return eCtx, nil
}

type executeOperationParams struct {
	ExecutionContext *executionContext
	Root             interface{}
	Operation        ast.Definition
}

func executeOperation(p executeOperationParams) *Result {
	operationType, err := getOperationRootType(p.ExecutionContext.Schema, p.Operation)
	if err != nil {
		return &Result{Errors: gqlerrors.FormatErrors(err)}
	}

	fields := collectFields(collectFieldsParams{
		ExeContext:   p.ExecutionContext,
		RuntimeType:  operationType,
		SelectionSet: p.Operation.GetSelectionSet(),
	})

	executeFieldsParams := executeFieldsParams{
		ExecutionContext: p.ExecutionContext,
		ParentType:       operationType,
		Source:           p.Root,
		Fields:           fields,
	}

	if p.Operation.GetOperation() == ast.OperationTypeMutation {
		return executeFieldsSerially(executeFieldsParams)
	}
	return executeFields(executeFieldsParams)

}

// Extracts the root type of the operation from the schema.
func getOperationRootType(schema Schema, operation ast.Definition) (*Object, error) {
	if operation == nil {
		return nil, errors.New("Can only execute queries and mutations")
	}

	switch operation.GetOperation() {
	case ast.OperationTypeQuery:
		return schema.QueryType(), nil
	case ast.OperationTypeMutation:
		mutationType := schema.MutationType()
		if mutationType.PrivateName == "" {
			return nil, gqlerrors.NewError(
				"Schema is not configured for mutations",
				[]ast.Node{operation},
				"",
				nil,
				[]int{},
				nil,
			)
		}
		return mutationType, nil
	case ast.OperationTypeSubscription:
		subscriptionType := schema.SubscriptionType()
		if subscriptionType.PrivateName == "" {
			return nil, gqlerrors.NewError(
				"Schema is not configured for subscriptions",
				[]ast.Node{operation},
				"",
				nil,
				[]int{},
				nil,
			)
		}
		return subscriptionType, nil
	default:
		return nil, gqlerrors.NewError(
			"Can only execute queries, mutations and subscription",
			[]ast.Node{operation},
			"",
			nil,
			[]int{},
			nil,
		)
	}
}

type executeFieldsParams struct {
	ExecutionContext *executionContext
	ParentType       *Object
	Source           interface{}
	Fields           map[string][]*ast.Field
}

// Implements the "Evaluating selection sets" section of the spec for "write" mode.
func executeFieldsSerially(p executeFieldsParams) *Result {
	if p.Source == nil {
		p.Source = map[string]interface{}{}
	}
	if p.Fields == nil {
		p.Fields = map[string][]*ast.Field{}
	}

	finalResults := make(map[string]interface{}, len(p.Fields))
	for responseName, fieldASTs := range p.Fields {
		resolved, state, err := resolveField(p.ExecutionContext, p.ParentType, p.Source, fieldASTs)
		if err != nil {
			 p.ExecutionContext.Errors = append(p.ExecutionContext.Errors, gqlerrors.FormatError(err))
		}
		if state.hasNoFieldDefs {
			continue
		}
		finalResults[responseName] = resolved
	}

	return &Result{
		Data:   finalResults,
		Errors: p.ExecutionContext.Errors,
	}
}

// Implements the "Evaluating selection sets" section of the spec for "read" mode.
func executeFields(p executeFieldsParams) *Result {
	if p.Source == nil {
		p.Source = map[string]interface{}{}
	}
	if p.Fields == nil {
		p.Fields = map[string][]*ast.Field{}
	}

	finalResults := make(map[string]interface{}, len(p.Fields))
	for responseName, fieldASTs := range p.Fields {
		resolved, state, err := resolveField(p.ExecutionContext, p.ParentType, p.Source, fieldASTs)
		if err != nil {
			p.ExecutionContext.Errors = append(p.ExecutionContext.Errors, gqlerrors.FormatError(err))
		}
		if state.hasNoFieldDefs {
			continue
		}
		finalResults[responseName] = resolved
	}

	return &Result{
		Data:   finalResults,
		Errors: p.ExecutionContext.Errors,
	}
}

type collectFieldsParams struct {
	ExeContext           *executionContext
	RuntimeType          *Object // previously known as OperationType
	SelectionSet         *ast.SelectionSet
	Fields               map[string][]*ast.Field
	VisitedFragmentNames map[string]bool
}

// Given a selectionSet, adds all of the fields in that selection to
// the passed in map of fields, and returns it at the end.
// CollectFields requires the "runtime type" of an object. For a field which
// returns and Interface or Union type, the "runtime type" will be the actual
// Object type returned by that field.
func collectFields(p collectFieldsParams) map[string][]*ast.Field {

	fields := p.Fields
	if fields == nil {
		fields = map[string][]*ast.Field{}
	}
	if p.VisitedFragmentNames == nil {
		p.VisitedFragmentNames = map[string]bool{}
	}
	if p.SelectionSet == nil {
		return fields
	}
	for _, iSelection := range p.SelectionSet.Selections {
		switch selection := iSelection.(type) {
		case *ast.Field:
			if !shouldIncludeNode(p.ExeContext, selection.Directives) {
				continue
			}
			name := getFieldEntryKey(selection)
			if _, ok := fields[name]; !ok {
				fields[name] = []*ast.Field{}
			}
			fields[name] = append(fields[name], selection)
		case *ast.InlineFragment:

			if !shouldIncludeNode(p.ExeContext, selection.Directives) ||
				!doesFragmentConditionMatch(p.ExeContext, selection, p.RuntimeType) {
				continue
			}
			innerParams := collectFieldsParams{
				ExeContext:           p.ExeContext,
				RuntimeType:          p.RuntimeType,
				SelectionSet:         selection.SelectionSet,
				Fields:               fields,
				VisitedFragmentNames: p.VisitedFragmentNames,
			}
			collectFields(innerParams)
		case *ast.FragmentSpread:
			fragName := ""
			if selection.Name != nil {
				fragName = selection.Name.Value
			}
			if visited, ok := p.VisitedFragmentNames[fragName]; (ok && visited) ||
				!shouldIncludeNode(p.ExeContext, selection.Directives) {
				continue
			}
			p.VisitedFragmentNames[fragName] = true
			fragment, hasFragment := p.ExeContext.Fragments[fragName]
			if !hasFragment {
				continue
			}

			if fragment, ok := fragment.(*ast.FragmentDefinition); ok {
				if !doesFragmentConditionMatch(p.ExeContext, fragment, p.RuntimeType) {
					continue
				}
				innerParams := collectFieldsParams{
					ExeContext:           p.ExeContext,
					RuntimeType:          p.RuntimeType,
					SelectionSet:         fragment.GetSelectionSet(),
					Fields:               fields,
					VisitedFragmentNames: p.VisitedFragmentNames,
				}
				collectFields(innerParams)
			}
		}
	}
	return fields
}

// Determines if a field should be included based on the @include and @skip
// directives, where @skip has higher precedence than @include.
func shouldIncludeNode(eCtx *executionContext, directives []*ast.Directive) bool {

	defaultReturnValue := true

	var skipAST *ast.Directive
	var includeAST *ast.Directive
	for _, directive := range directives {
		if directive == nil || directive.Name == nil {
			continue
		}
		if directive.Name.Value == SkipDirective.Name {
			skipAST = directive
			break
		}
	}
	if skipAST != nil {
		argValues, err := getArgumentValues(
			SkipDirective.Args,
			skipAST.Arguments,
			eCtx.VariableValues,
		)
		if err != nil {
			return defaultReturnValue
		}
		if skipIf, ok := argValues["if"].(bool); ok {
			if skipIf == true {
				return false
			}
		}
	}
	for _, directive := range directives {
		if directive == nil || directive.Name == nil {
			continue
		}
		if directive.Name.Value == IncludeDirective.Name {
			includeAST = directive
			break
		}
	}
	if includeAST != nil {
		argValues, err := getArgumentValues(
			IncludeDirective.Args,
			includeAST.Arguments,
			eCtx.VariableValues,
		)
		if err != nil {
			return defaultReturnValue
		}
		if includeIf, ok := argValues["if"].(bool); ok {
			if includeIf == false {
				return false
			}
		}
	}
	return defaultReturnValue
}

// Determines if a fragment is applicable to the given type.
func doesFragmentConditionMatch(eCtx *executionContext, fragment ast.Node, ttype *Object) bool {

	switch fragment := fragment.(type) {
	case *ast.FragmentDefinition:
		typeConditionAST := fragment.TypeCondition
		if typeConditionAST == nil {
			return true
		}
		conditionalType, err := typeFromAST(eCtx.Schema, typeConditionAST)
		if err != nil {
			return false
		}
		if conditionalType == ttype {
			return true
		}
		if conditionalType.Name() == ttype.Name() {
			return true
		}
		if conditionalType, ok := conditionalType.(*Interface); ok {
			return eCtx.Schema.IsPossibleType(conditionalType, ttype)
		}
		if conditionalType, ok := conditionalType.(*Union); ok {
			return eCtx.Schema.IsPossibleType(conditionalType, ttype)
		}
	case *ast.InlineFragment:
		typeConditionAST := fragment.TypeCondition
		if typeConditionAST == nil {
			return true
		}
		conditionalType, err := typeFromAST(eCtx.Schema, typeConditionAST)
		if err != nil {
			return false
		}
		if conditionalType == ttype {
			return true
		}
		if conditionalType.Name() == ttype.Name() {
			return true
		}
		if conditionalType, ok := conditionalType.(*Interface); ok {
			return eCtx.Schema.IsPossibleType(conditionalType, ttype)
		}
		if conditionalType, ok := conditionalType.(*Union); ok {
			return eCtx.Schema.IsPossibleType(conditionalType, ttype)
		}
	}

	return false
}

// Implements the logic to compute the key of a given field’s entry
func getFieldEntryKey(node *ast.Field) string {

	if node.Alias != nil && node.Alias.Value != "" {
		return node.Alias.Value
	}
	if node.Name != nil && node.Name.Value != "" {
		return node.Name.Value
	}
	return ""
}

// Internal resolveField state
type resolveFieldResultState struct {
	hasNoFieldDefs bool
}

// Resolves the field on the given source object. In particular, this
// figures out the value that the field returns by calling its resolve function,
// then calls completeValue to complete promises, serialize scalars, or execute
// the sub-selection-set for objects.
func resolveField(eCtx *executionContext, parentType *Object, source interface{}, fieldASTs []*ast.Field) (result interface{}, resultState resolveFieldResultState, err error) {
	// catch panic from resolveFn
	var returnType Output
	defer func() (interface{}, resolveFieldResultState) {
		if r := recover(); r != nil {
			if eCtx.PanicHandler != nil {
				eCtx.PanicHandler(eCtx.Context, r)
			}
			var err error
			if r, ok := r.(string); ok {
				err = NewLocatedError(
					fmt.Sprintf("%v", r),
					FieldASTsToNodeASTs(fieldASTs),
				)
			}
			if r, ok := r.(error); ok {
				err = gqlerrors.FormatError(r)
			}
			// send panic upstream
			if _, ok := returnType.(*NonNull); ok {
				panic(gqlerrors.FormatError(err))
			}
			eCtx.Errors = append(eCtx.Errors, gqlerrors.FormatError(err))
			return result, resultState
		}
		return result, resultState
	}()

	fieldAST := fieldASTs[0]
	fieldName := ""
	if fieldAST.Name != nil {
		fieldName = fieldAST.Name.Value
	}

	fieldDef := getFieldDef(eCtx.Schema, parentType, fieldName)
	if fieldDef == nil {
		resultState.hasNoFieldDefs = true
		return nil, resultState, nil
	}
	returnType = fieldDef.Type
	resolveFn := fieldDef.Resolve
	if resolveFn == nil {
		resolveFn = DefaultResolveFn
	}

	// Build a map of arguments from the field.arguments AST, using the
	// variables scope to fulfill any variable references.
	// TODO: find a way to memoize, in case this field is within a List type.
	args, _ := getArgumentValues(fieldDef.Args, fieldAST.Arguments, eCtx.VariableValues)

	info := ResolveInfo{
		FieldName:      fieldName,
		FieldASTs:      fieldASTs,
		ReturnType:     returnType,
		ParentType:     parentType,
		Schema:         eCtx.Schema,
		Fragments:      eCtx.Fragments,
		RootValue:      eCtx.Root,
		Operation:      eCtx.Operation,
		VariableValues: eCtx.VariableValues,
	}

	var resolveFnError error

	result, resolveFnError = resolveFn(ResolveParams{
		Source:  source,
		Args:    args,
		Info:    info,
		Context: eCtx.Context,
	})

	completed := completeValueCatchingError(eCtx, returnType, fieldASTs, info, result)
	return completed, resultState, resolveFnError
}

func completeValueCatchingError(eCtx *executionContext, returnType Type, fieldASTs []*ast.Field, info ResolveInfo, result interface{}) (completed interface{}) {
	// catch panic
	defer func() interface{} {
		if r := recover(); r != nil {
			//send panic upstream
			if _, ok := returnType.(*NonNull); ok {
				panic(r)
			}
			if err, ok := r.(gqlerrors.FormattedError); ok {
				eCtx.Errors = append(eCtx.Errors, err)
			}
			return completed
		}
		return completed
	}()

	if returnType, ok := returnType.(*NonNull); ok {
		completed := completeValue(eCtx, returnType, fieldASTs, info, result)
		return completed
	}
	completed = completeValue(eCtx, returnType, fieldASTs, info, result)
	return completed
}

func completeValue(eCtx *executionContext, returnType Type, fieldASTs []*ast.Field, info ResolveInfo, result interface{}) interface{} {

	resultVal := reflect.ValueOf(result)
	if resultVal.IsValid() && resultVal.Type().Kind() == reflect.Func {
		if propertyFn, ok := result.(func() interface{}); ok {
			return propertyFn()
		}
		err := gqlerrors.NewFormattedError("Error resolving func. Expected `func() interface{}` signature")
		panic(gqlerrors.FormatError(err))
	}

	// If field type is NonNull, complete for inner type, and throw field error
	// if result is null.
	if returnType, ok := returnType.(*NonNull); ok {
		completed := completeValue(eCtx, returnType.OfType, fieldASTs, info, result)
		if completed == nil {
			err := NewLocatedError(
				fmt.Sprintf("Cannot return null for non-nullable field %v.%v.", info.ParentType, info.FieldName),
				FieldASTsToNodeASTs(fieldASTs),
			)
			panic(gqlerrors.FormatError(err))
		}
		return completed
	}

	// If result value is null-ish (null, undefined, or NaN) then return null.
	if isNullish(result) {
		return nil
	}

	// If field type is List, complete each item in the list with the inner type
	if returnType, ok := returnType.(*List); ok {
		return completeListValue(eCtx, returnType, fieldASTs, info, result)
	}

	// If field type is a leaf type, Scalar or Enum, serialize to a valid value,
	// returning null if serialization is not possible.
	if returnType, ok := returnType.(*Scalar); ok {
		return completeLeafValue(returnType, result)
	}
	if returnType, ok := returnType.(*Enum); ok {
		return completeLeafValue(returnType, result)
	}

	// If field type is an abstract type, Interface or Union, determine the
	// runtime Object type and complete for that type.
	if returnType, ok := returnType.(*Union); ok {
		return completeAbstractValue(eCtx, returnType, fieldASTs, info, result)
	}
	if returnType, ok := returnType.(*Interface); ok {
		return completeAbstractValue(eCtx, returnType, fieldASTs, info, result)
	}

	// If field type is Object, execute and complete all sub-selections.
	if returnType, ok := returnType.(*Object); ok {
		return completeObjectValue(eCtx, returnType, fieldASTs, info, result)
	}

	// Not reachable. All possible output types have been considered.
	err := invariantf(false,
		`Cannot complete value of unexpected type "%v."`, returnType)

	if err != nil {
		panic(gqlerrors.FormatError(err))
	}
	return nil
}

// completeAbstractValue completes value of an Abstract type (Union / Interface) by determining the runtime type
// of that value, then completing based on that type.
func completeAbstractValue(eCtx *executionContext, returnType Abstract, fieldASTs []*ast.Field, info ResolveInfo, result interface{}) interface{} {

	var runtimeType *Object

	resolveTypeParams := ResolveTypeParams{
		Value:   result,
		Info:    info,
		Context: eCtx.Context,
	}
	if unionReturnType, ok := returnType.(*Union); ok && unionReturnType.ResolveType != nil {
		runtimeType = unionReturnType.ResolveType(resolveTypeParams)
	} else if interfaceReturnType, ok := returnType.(*Interface); ok && interfaceReturnType.ResolveType != nil {
		runtimeType = interfaceReturnType.ResolveType(resolveTypeParams)
	} else {
		runtimeType = defaultResolveTypeFn(resolveTypeParams, returnType)
	}

	err := invariant(runtimeType != nil,
		fmt.Sprintf(`Abstract type %v must resolve to an Object type at runtime `+
			`for field %v.%v with value "%v", received "%v".`,
			returnType, info.ParentType, info.FieldName, result, runtimeType),
	)
	if err != nil {
		panic(err)
	}

	if !eCtx.Schema.IsPossibleType(returnType, runtimeType) {
		panic(gqlerrors.NewFormattedError(
			fmt.Sprintf(`Runtime Object type "%v" is not a possible type `+
				`for "%v".`, runtimeType, returnType),
		))
	}

	return completeObjectValue(eCtx, runtimeType, fieldASTs, info, result)
}

// completeObjectValue complete an Object value by executing all sub-selections.
func completeObjectValue(eCtx *executionContext, returnType *Object, fieldASTs []*ast.Field, info ResolveInfo, result interface{}) interface{} {

	// If there is an isTypeOf predicate function, call it with the
	// current result. If isTypeOf returns false, then raise an error rather
	// than continuing execution.
	if returnType.IsTypeOf != nil {
		p := IsTypeOfParams{
			Value:   result,
			Info:    info,
			Context: eCtx.Context,
		}
		if !returnType.IsTypeOf(p) {
			panic(gqlerrors.NewFormattedError(
				fmt.Sprintf(`Expected value of type "%v" but got: %T.`, returnType, result),
			))
		}
	}

	// Collect sub-fields to execute to complete this value.
	subFieldASTs := map[string][]*ast.Field{}
	visitedFragmentNames := map[string]bool{}
	for _, fieldAST := range fieldASTs {
		if fieldAST == nil {
			continue
		}
		selectionSet := fieldAST.SelectionSet
		if selectionSet != nil {
			innerParams := collectFieldsParams{
				ExeContext:           eCtx,
				RuntimeType:          returnType,
				SelectionSet:         selectionSet,
				Fields:               subFieldASTs,
				VisitedFragmentNames: visitedFragmentNames,
			}
			subFieldASTs = collectFields(innerParams)
		}
	}
	executeFieldsParams := executeFieldsParams{
		ExecutionContext: eCtx,
		ParentType:       returnType,
		Source:           result,
		Fields:           subFieldASTs,
	}
	results := executeFields(executeFieldsParams)

	return results.Data

}

// completeLeafValue complete a leaf value (Scalar / Enum) by serializing to a valid value, returning nil if serialization is not possible.
func completeLeafValue(returnType Leaf, result interface{}) interface{} {
	serializedResult := returnType.Serialize(result)
	if isNullish(serializedResult) {
		return nil
	}
	return serializedResult
}

// completeListValue complete a list value by completing each item in the list with the inner type
func completeListValue(eCtx *executionContext, returnType *List, fieldASTs []*ast.Field, info ResolveInfo, result interface{}) interface{} {
	resultVal := reflect.ValueOf(result)
	parentTypeName := ""
	if info.ParentType != nil {
		parentTypeName = info.ParentType.Name()
	}
	err := invariantf(
		resultVal.IsValid() && resultVal.Type().Kind() == reflect.Slice,
		"User Error: expected iterable, but did not find one "+
			"for field %v.%v.", parentTypeName, info.FieldName)

	if err != nil {
		panic(gqlerrors.FormatError(err))
	}

	itemType := returnType.OfType
	completedResults := make([]interface{}, 0, resultVal.Len())
	for i := 0; i < resultVal.Len(); i++ {
		val := resultVal.Index(i).Interface()
		completedItem := completeValueCatchingError(eCtx, itemType, fieldASTs, info, val)
		completedResults = append(completedResults, completedItem)
	}
	return completedResults
}

// defaultResolveTypeFn If a resolveType function is not given, then a default resolve behavior is
// used which tests each possible type for the abstract type by calling
// isTypeOf for the object being coerced, returning the first type that matches.
func defaultResolveTypeFn(p ResolveTypeParams, abstractType Abstract) *Object {
	possibleTypes := p.Info.Schema.PossibleTypes(abstractType)
	for _, possibleType := range possibleTypes {
		if possibleType.IsTypeOf == nil {
			continue
		}
		isTypeOfParams := IsTypeOfParams{
			Value:   p.Value,
			Info:    p.Info,
			Context: p.Context,
		}
		if res := possibleType.IsTypeOf(isTypeOfParams); res {
			return possibleType
		}
	}
	return nil
}

// FieldResolver is used in DefaultResolveFn when the the source value implements this interface.
type FieldResolver interface {
	// Resolve resolves the value for the given ResolveParams. It has the same semantics as FieldResolveFn.
	Resolve(p ResolveParams) (interface{}, error)
}

// defaultResolveFn If a resolve function is not given, then a default resolve behavior is used
// which takes the property of the source object of the same name as the field
// and returns it as the result, or if it's a function, returns the result
// of calling that function.
func DefaultResolveFn(p ResolveParams) (interface{}, error) {
	sourceVal := reflect.ValueOf(p.Source)
	// Check if value implements 'Resolver' interface
	if resolver, ok := sourceVal.Interface().(FieldResolver); ok {
		return resolver.Resolve(p)
	}

	// try to resolve p.Source as a struct
	if sourceVal.IsValid() && sourceVal.Type().Kind() == reflect.Ptr {
		sourceVal = sourceVal.Elem()
	}
	if !sourceVal.IsValid() {
		return nil, nil
	}

	if sourceVal.Type().Kind() == reflect.Struct {
		for i := 0; i < sourceVal.NumField(); i++ {
			valueField := sourceVal.Field(i)
			typeField := sourceVal.Type().Field(i)
			// try matching the field name first
			if strings.EqualFold(typeField.Name, p.Info.FieldName) {
				return valueField.Interface(), nil
			}
			tag := typeField.Tag
			checkTag := func(tagName string) bool {
				t := tag.Get(tagName)
				tOptions := strings.Split(t, ",")
				if len(tOptions) == 0 {
					return false
				}
				if tOptions[0] != p.Info.FieldName {
					return false
				}
				return true
			}
			if checkTag("json") || checkTag("graphql") {
				return valueField.Interface(), nil
			} else {
				continue
			}
		}
		return nil, nil
	}

	// try p.Source as a map[string]interface
	if sourceMap, ok := p.Source.(map[string]interface{}); ok {
		property := sourceMap[p.Info.FieldName]
		val := reflect.ValueOf(property)
		if val.IsValid() && val.Type().Kind() == reflect.Func {
			// try type casting the func to the most basic func signature
			// for more complex signatures, user have to define ResolveFn
			if propertyFn, ok := property.(func() interface{}); ok {
				return propertyFn(), nil
			}
		}
		return property, nil
	}

	// last resort, return nil
	return nil, nil
}

// This method looks up the field on the given type definition.
// It has special casing for the two introspection fields, __schema
// and __typename. __typename is special because it can always be
// queried as a field, even in situations where no other fields
// are allowed, like on a Union. __schema could get automatically
// added to the query type, but that would require mutating type
// definitions, which would cause issues.
func getFieldDef(schema Schema, parentType *Object, fieldName string) *FieldDefinition {

	if parentType == nil {
		return nil
	}

	if fieldName == SchemaMetaFieldDef.Name &&
		schema.QueryType() == parentType {
		return SchemaMetaFieldDef
	}
	if fieldName == TypeMetaFieldDef.Name &&
		schema.QueryType() == parentType {
		return TypeMetaFieldDef
	}
	if fieldName == TypeNameMetaFieldDef.Name {
		return TypeNameMetaFieldDef
	}
	return parentType.Fields()[fieldName]
}
