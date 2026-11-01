// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"reflect"
	"sync"
	"unsafe"

	"github.com/pkg/errors"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// drainContext replicates (ctx *sdk.Context).wait() — the unexported method
// RunWithContext calls to drain pending async work before program exit.
//
// Without this, a bare sdk.NewContext + direct fn() call returns before
// Pulumi's background RegisterResource goroutines have talked to the
// monitor, so the engine sees zero sub-resources and plans a destroy of
// every entry in state. RunWithContext would wait on the right machinery,
// but it also registers a root `pulumi:pulumi:Stack` whose URN is
// `<project>-<stack>` — byte-identical to the one SC core already pushed
// into the monitor proxy from its own pctx, so Pulumi rejects the second
// register with "Duplicate resource URN".
//
// So we reach into pulumi/sdk/v3/go/pulumi/context.go's contextState via
// reflect+unsafe (same pattern pkg/plugin/monitor_proxy.go uses to lift
// the engine's monitor client) and run the wait sequence by hand:
//
//  1. state.join.Wait()        — non-RPC async work (ApplyT closures)
//  2. state.rpcs → 0 under state.rpcsLock via state.rpcsDone.Wait()
//
// If a future Pulumi SDK renames contextState or any of these fields,
// drainContext returns a diagnostic rather than nil-dereffing, so a
// regression is caught at plugin boot and not three deploys later.
func drainContext(ctx *sdk.Context) error {
	if ctx == nil {
		return errors.New("nil pulumi.Context")
	}
	cv := reflect.ValueOf(ctx).Elem()
	stateField := cv.FieldByName("state")
	if !stateField.IsValid() || stateField.Kind() != reflect.Ptr {
		return errors.New("pulumi.Context has no `state` pointer (SDK renamed?)")
	}
	stateVal := stateField.Elem()
	if stateVal.Kind() != reflect.Struct {
		return errors.New("pulumi.Context.state is not a struct (SDK renamed?)")
	}

	// 1) join.Wait()
	joinField := stateVal.FieldByName("join")
	if !joinField.IsValid() {
		return errors.New("pulumi.Context.state has no `join` field (SDK renamed?)")
	}
	joinAddr := reflect.NewAt(joinField.Type(), unsafe.Pointer(joinField.UnsafeAddr()))
	waitMethod := joinAddr.MethodByName("Wait")
	if !waitMethod.IsValid() {
		return errors.New("pulumi.Context.state.join has no Wait method (SDK renamed?)")
	}
	waitMethod.Call(nil)

	// 2) rpcs → 0 under rpcsLock via rpcsDone
	rpcsLockField := stateVal.FieldByName("rpcsLock")
	rpcsField := stateVal.FieldByName("rpcs")
	rpcsDoneField := stateVal.FieldByName("rpcsDone")
	if !rpcsLockField.IsValid() || !rpcsField.IsValid() || !rpcsDoneField.IsValid() {
		return errors.New("pulumi.Context.state has no rpcs/rpcsLock/rpcsDone (SDK renamed?)")
	}
	lockPtr := (*sync.Mutex)(unsafe.Pointer(rpcsLockField.UnsafeAddr()))
	rpcsPtr := (*int)(unsafe.Pointer(rpcsField.UnsafeAddr()))
	rpcsDonePtr := (**sync.Cond)(unsafe.Pointer(rpcsDoneField.UnsafeAddr()))

	lockPtr.Lock()
	defer lockPtr.Unlock()
	for *rpcsPtr > 0 {
		cond := *rpcsDonePtr
		if cond == nil {
			// The field is only initialised the first time a goroutine
			// decrements it to zero; if we beat that lazily, create a
			// cond keyed on our lock so Wait yields the mutex properly.
			cond = sync.NewCond(lockPtr)
			*rpcsDonePtr = cond
		}
		cond.Wait()
	}
	return nil
}
