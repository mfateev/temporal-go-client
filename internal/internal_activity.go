// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package internal

// All code in this file is private to the package.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/opentracing/opentracing-go"
	"github.com/uber-go/tally"
	commonpb "go.temporal.io/api/common/v1"

	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/log"
)

type (
	// activity is an interface of an activity implementation.
	activity interface {
		Execute(ctx context.Context, input *commonpb.Payloads) (*commonpb.Payloads, error)
		ActivityType() ActivityType
		GetFunction() interface{}
	}

	// ActivityID uniquely identifies an activity execution
	ActivityID struct {
		id string
	}

	// LocalActivityID uniquely identifies a local activity execution
	LocalActivityID struct {
		id string
	}

	// ExecuteActivityOptions option for executing an activity
	ExecuteActivityOptions struct {
		ActivityID             string // Users can choose IDs but our framework makes it optional to decrease the crust.
		TaskQueueName          string
		ScheduleToCloseTimeout time.Duration
		ScheduleToStartTimeout time.Duration
		StartToCloseTimeout    time.Duration
		HeartbeatTimeout       time.Duration
		WaitForCancellation    bool
		OriginalTaskQueueName  string
		RetryPolicy            *commonpb.RetryPolicy
	}

	// ExecuteLocalActivityOptions options for executing a local activity
	ExecuteLocalActivityOptions struct {
		ScheduleToCloseTimeout time.Duration
		StartToCloseTimeout    time.Duration
		RetryPolicy            *RetryPolicy
	}

	// ExecuteActivityParams parameters for executing an activity
	ExecuteActivityParams struct {
		ExecuteActivityOptions
		ActivityType  ActivityType
		Input         *commonpb.Payloads
		DataConverter converter.DataConverter
		Header        *commonpb.Header
	}

	// ExecuteLocalActivityParams parameters for executing a local activity
	ExecuteLocalActivityParams struct {
		ExecuteLocalActivityOptions
		ActivityFn    interface{} // local activity function pointer
		ActivityType  string      // local activity type
		InputArgs     []interface{}
		WorkflowInfo  *WorkflowInfo
		DataConverter converter.DataConverter
		Attempt       int32
		ScheduledTime time.Time
		Header        *commonpb.Header
	}

	// AsyncActivityClient for requesting activity execution
	AsyncActivityClient interface {
		// The ExecuteActivity schedules an activity with a callback handler.
		// If the activity failed to complete the callback error would indicate the failure
		// and it can be one of ActivityTaskFailedError, ActivityTaskTimeoutError, ActivityTaskCanceledError
		ExecuteActivity(parameters ExecuteActivityParams, callback ResultHandler) ActivityID

		// This only initiates cancel request for activity. if the activity is configured to not WaitForCancellation then
		// it would invoke the callback handler immediately with error code ActivityTaskCanceledError.
		// If the activity is not running(either scheduled or started) then it is a no-operation.
		RequestCancelActivity(activityID ActivityID)
	}

	// LocalActivityClient for requesting local activity execution
	LocalActivityClient interface {
		ExecuteLocalActivity(params ExecuteLocalActivityParams, callback LocalActivityResultHandler) LocalActivityID

		RequestCancelLocalActivity(activityID LocalActivityID)
	}

	activityEnvironment struct {
		taskToken          []byte
		workflowExecution  WorkflowExecution
		activityID         string
		activityType       ActivityType
		serviceInvoker     ServiceInvoker
		logger             log.Logger
		metricsScope       tally.Scope
		isLocalActivity    bool
		heartbeatTimeout   time.Duration
		deadline           time.Time
		scheduledTime      time.Time
		startedTime        time.Time
		taskQueue          string
		dataConverter      converter.DataConverter
		attempt            int32 // starts from 1.
		heartbeatDetails   *commonpb.Payloads
		workflowType       *WorkflowType
		workflowNamespace  string
		workerStopChannel  <-chan struct{}
		contextPropagators []ContextPropagator
		tracer             opentracing.Tracer
	}

	// context.WithValue need this type instead of basic type string to avoid lint error
	contextKey string
)

const (
	activityEnvContextKey          contextKey = "activityEnv"
	activityOptionsContextKey      contextKey = "activityOptions"
	localActivityOptionsContextKey contextKey = "localActivityOptions"
)

func (i ActivityID) String() string {
	return i.id
}

// ParseActivityID returns ActivityID constructed from its string representation.
// The string representation should be obtained through ActivityID.String()
func ParseActivityID(id string) (ActivityID, error) {
	return ActivityID{id: id}, nil
}

func (i LocalActivityID) String() string {
	return i.id
}

// ParseLocalActivityID returns LocalActivityID constructed from its string representation.
// The string representation should be obtained through LocalActivityID.String()
func ParseLocalActivityID(v string) (LocalActivityID, error) {
	return LocalActivityID{id: v}, nil
}

func getActivityEnv(ctx context.Context) *activityEnvironment {
	env := ctx.Value(activityEnvContextKey)
	if env == nil {
		panic("getActivityEnv: Not an activity context")
	}
	return env.(*activityEnvironment)
}

func getActivityOptions(ctx Context) *ExecuteActivityOptions {
	eap := ctx.Value(activityOptionsContextKey)
	if eap == nil {
		return nil
	}
	return eap.(*ExecuteActivityOptions)
}

func getLocalActivityOptions(ctx Context) *ExecuteLocalActivityOptions {
	opts := ctx.Value(localActivityOptionsContextKey)
	if opts == nil {
		return nil
	}
	return opts.(*ExecuteLocalActivityOptions)
}

func getValidatedLocalActivityOptions(ctx Context) (*ExecuteLocalActivityOptions, error) {
	p := getLocalActivityOptions(ctx)
	if p == nil {
		return nil, errLocalActivityParamsBadRequest
	}
	if p.ScheduleToCloseTimeout < 0 {
		return nil, errors.New("negative ScheduleToCloseTimeout")
	}
	if p.StartToCloseTimeout < 0 {
		return nil, errors.New("negative StartToCloseTimeout")
	}
	if p.ScheduleToCloseTimeout == 0 && p.StartToCloseTimeout == 0 {
		return nil, errors.New("at least one of ScheduleToCloseTimeout and StartToCloseTimeout is required")
	}
	if p.ScheduleToCloseTimeout == 0 {
		p.ScheduleToCloseTimeout = p.StartToCloseTimeout
	} else {
		p.StartToCloseTimeout = p.ScheduleToCloseTimeout
	}
	return p, nil
}

func validateFunctionArgs(workflowFunc interface{}, args []interface{}, isWorkflow bool) error {
	fType := reflect.TypeOf(workflowFunc)
	switch getKind(fType) {
	case reflect.String:
		// We can't validate function passed as string.
		return nil
	case reflect.Func:
	default:
		return fmt.Errorf(
			"invalid type 'workflowFunc' parameter provided, it can be either worker function or function name: %v",
			workflowFunc)
	}

	fnName, _ := getFunctionName(workflowFunc)
	fnArgIndex := 0
	// Skip Context function argument.
	if fType.NumIn() > 0 {
		if isWorkflow && isWorkflowContext(fType.In(0)) {
			fnArgIndex++
		}
		if !isWorkflow && isActivityContext(fType.In(0)) {
			fnArgIndex++
		}
	}

	// Validate provided args match with function order match.
	if fType.NumIn()-fnArgIndex != len(args) {
		return fmt.Errorf(
			"expected %d args for function: %v but found %v",
			fType.NumIn()-fnArgIndex, fnName, len(args))
	}

	for i := 0; fnArgIndex < fType.NumIn(); fnArgIndex, i = fnArgIndex+1, i+1 {
		fnArgType := fType.In(fnArgIndex)
		argType := reflect.TypeOf(args[i])
		if argType != nil && !argType.AssignableTo(fnArgType) {
			return fmt.Errorf(
				"cannot assign function argument: %d from type: %s to type: %s",
				fnArgIndex+1, argType, fnArgType,
			)
		}
	}

	return nil
}

func getValidatedActivityFunction(f interface{}, args []interface{}, registry *registry) (*ActivityType, error) {
	fnName := ""
	fType := reflect.TypeOf(f)
	switch getKind(fType) {
	case reflect.String:
		fnName = reflect.ValueOf(f).String()
	case reflect.Func:
		if err := validateFunctionArgs(f, args, false); err != nil {
			return nil, err
		}
		fnName, _ = getFunctionName(f)
		if alias, ok := registry.getActivityAlias(fnName); ok {
			fnName = alias
		}

	default:
		return nil, fmt.Errorf(
			"invalid type 'f' parameter provided, it can be either activity function or name of the activity: %v", f)
	}

	return &ActivityType{Name: fnName}, nil
}

func getKind(fType reflect.Type) reflect.Kind {
	if fType == nil {
		return reflect.Invalid
	}
	return fType.Kind()
}

func isActivityContext(inType reflect.Type) bool {
	contextElem := reflect.TypeOf((*context.Context)(nil)).Elem()
	return inType != nil && inType.Implements(contextElem)
}

func validateFunctionAndGetResults(f interface{}, values []reflect.Value, dataConverter converter.DataConverter) (*commonpb.Payloads, error) {
	resultSize := len(values)

	if resultSize < 1 || resultSize > 2 {
		fnName, _ := getFunctionName(f)
		return nil, fmt.Errorf(
			"the function: %v signature returns %d results, it is expecting to return either error or (result, error)",
			fnName, resultSize)
	}

	var result *commonpb.Payloads

	// Parse result
	if resultSize > 1 {
		retValue := values[0]

		var ok bool
		if result, ok = retValue.Interface().(*commonpb.Payloads); !ok {
			if retValue.Kind() != reflect.Ptr || !retValue.IsNil() {
				var err error
				if result, err = encodeArg(dataConverter, retValue.Interface()); err != nil {
					return nil, err
				}
			}
		}
	}

	// Parse error.
	errValue := values[resultSize-1]
	if errValue.IsNil() {
		return result, nil
	}
	errInterface, ok := errValue.Interface().(error)
	if !ok {
		return nil, fmt.Errorf(
			"failed to parse error result as it is not of error interface: %v",
			errValue)
	}
	return result, errInterface
}

func serializeResults(f interface{}, results []interface{}, dataConverter converter.DataConverter) (result *commonpb.Payloads, err error) {
	// results contain all results including error
	resultSize := len(results)

	if resultSize < 1 || resultSize > 2 {
		fnName, _ := getFunctionName(f)
		err = fmt.Errorf(
			"the function: %v signature returns %d results, it is expecting to return either error or (result, error)",
			fnName, resultSize)
		return
	}
	if resultSize > 1 {
		retValue := results[0]
		if retValue != nil {
			result, err = encodeArg(dataConverter, retValue)
			if err != nil {
				return nil, err
			}
		}
	}
	errResult := results[resultSize-1]
	if errResult != nil {
		var ok bool
		err, ok = errResult.(error)
		if !ok {
			err = fmt.Errorf(
				"failed to serialize error result as it is not of error interface: %v",
				errResult)
		}
	}
	return
}

func setActivityParametersIfNotExist(ctx Context) Context {
	params := getActivityOptions(ctx)
	var newParams ExecuteActivityOptions
	if params != nil {
		newParams = *params
		if params.RetryPolicy != nil {
			newRetryPolicy := *newParams.RetryPolicy
			newParams.RetryPolicy = &newRetryPolicy
		}
	}
	return WithValue(ctx, activityOptionsContextKey, &newParams)
}

func setLocalActivityParametersIfNotExist(ctx Context) Context {
	params := getLocalActivityOptions(ctx)
	var newParams ExecuteLocalActivityOptions
	if params != nil {
		newParams = *params
	}
	return WithValue(ctx, localActivityOptionsContextKey, &newParams)
}
