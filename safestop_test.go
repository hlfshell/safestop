package safestop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSafestop(t *testing.T) {
	//Channels needed to pass
	completeChan := make(chan bool, 1)
	errorChan := make(chan bool, 1)
	firstHandlerChan := make(chan bool, 1)
	secondHandlerChan := make(chan bool, 1)

	//IDs for handlers
	idFirst := "first-handler"
	idSecond := "second-handler"

	//Create on complete and on error funcs
	onComplete := func(errors map[string]error) {
		//We are expecting errors to not be nil
		require.NotNil(t, errors)

		assert.Equal(t, 2, len(errors))
		assert.Nil(t, errors[idFirst])
		assert.NotNil(t, errors[idSecond])
		completeChan <- true
	}

	onError := func(id string, err error) {
		assert.Equal(t, idSecond, id)
		assert.NotNil(t, err)
		errorChan <- true
	}

	//Setup a safestop
	ss := NewSafeStop(&Opts{
		Signals:    nil,
		OnComplete: onComplete,
		OnError:    onError,
		Timeout:    time.Minute,
	})

	ss.RegisterHandler(idFirst, func(ctx context.Context) error {
		//Be successful, so just return nothing
		firstHandlerChan <- true
		return nil
	})

	ss.RegisterHandler(idSecond, func(ctx context.Context) error {
		//Fail so we can test error handling
		secondHandlerChan <- true
		return errors.New("Failed because we wanted it to")
	})

	//We will now trigger shutdown using the test helper
	ss.TriggerShutdown(syscall.SIGINT)

	//All of these channels should pass. If we time out, we fail
	<-firstHandlerChan
	<-secondHandlerChan
	<-errorChan
	<-completeChan
}

func TestConcurrentExecution(t *testing.T) {
	// This test verifies that handlers actually run concurrently
	// by using timing to detect sequential vs concurrent execution
	handlerCount := 5
	handlerDuration := 100 * time.Millisecond

	// If handlers run sequentially, total time would be handlerCount * handlerDuration
	// If concurrent, total time should be approximately handlerDuration
	maxExpectedTime := time.Duration(handlerCount) * handlerDuration / 2

	startTimes := make([]time.Time, handlerCount)
	endTimes := make([]time.Time, handlerCount)
	var mu sync.Mutex

	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	for i := 0; i < handlerCount; i++ {
		id := fmt.Sprintf("handler-%d", i)
		i := i // capture loop variable
		ss.RegisterHandler(id, func(ctx context.Context) error {
			mu.Lock()
			startTimes[i] = time.Now()
			mu.Unlock()

			time.Sleep(handlerDuration)

			mu.Lock()
			endTimes[i] = time.Now()
			mu.Unlock()
			return nil
		})
	}

	start := time.Now()
	ss.TriggerShutdown(syscall.SIGTERM)
	ss.Wait()
	elapsed := time.Since(start)

	// Verify handlers ran concurrently (should take much less than sequential time)
	assert.Less(t, elapsed, maxExpectedTime, "Handlers should run concurrently")

	// Verify all handlers started around the same time
	mu.Lock()
	firstStart := startTimes[0]
	lastStart := startTimes[handlerCount-1]
	mu.Unlock()

	startSpread := lastStart.Sub(firstStart)
	assert.Less(t, startSpread, 50*time.Millisecond, "Handlers should start nearly simultaneously")
}

func TestNilOpts(t *testing.T) {
	// Test that nil opts doesn't panic
	ss := NewSafeStop(nil)
	require.NotNil(t, ss)

	// Should have default signals
	assert.Equal(t, 0, ss.GetHandlerCount())

	// Should be able to register handlers
	ss.RegisterHandler("test", func(ctx context.Context) error {
		return nil
	})
	assert.Equal(t, 1, ss.GetHandlerCount())
}

func TestDuplicateRegistration(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	callCount := 0
	handler1 := func(ctx context.Context) error {
		callCount++
		return nil
	}

	handler2 := func(ctx context.Context) error {
		callCount++
		return nil
	}

	// Register same ID twice
	ss.RegisterHandler("duplicate", handler1)
	ss.RegisterHandler("duplicate", handler2)

	assert.Equal(t, 1, ss.GetHandlerCount())

	// Trigger shutdown - should only call the second handler
	ss.TriggerShutdown(syscall.SIGTERM)
	ss.Wait()

	assert.Equal(t, 1, callCount, "Only the last registered handler should be called")
}

func TestUnregisterHandler(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	executed := make(map[string]bool)
	ss.RegisterHandler("handler1", func(ctx context.Context) error {
		executed["handler1"] = true
		return nil
	})
	ss.RegisterHandler("handler2", func(ctx context.Context) error {
		executed["handler2"] = true
		return nil
	})

	assert.Equal(t, 2, ss.GetHandlerCount())

	// Unregister one handler
	removed := ss.UnregisterHandler("handler1")
	assert.True(t, removed)
	assert.Equal(t, 1, ss.GetHandlerCount())

	// Try to unregister non-existent handler
	removed = ss.UnregisterHandler("nonexistent")
	assert.False(t, removed)

	// Trigger shutdown
	ss.TriggerShutdown(syscall.SIGTERM)
	ss.Wait()

	// Only handler2 should have executed
	assert.True(t, executed["handler2"])
	assert.False(t, executed["handler1"])
}

func TestManualShutdown(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	executed := false
	ss.RegisterHandler("test", func(ctx context.Context) error {
		executed = true
		return nil
	})

	// Manually trigger shutdown
	ss.Shutdown()
	ss.Wait()

	assert.True(t, executed, "Handler should have been executed")
}

func TestWaitMethod(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	handlerDone := make(chan bool, 1)
	ss.RegisterHandler("test", func(ctx context.Context) error {
		time.Sleep(50 * time.Millisecond)
		handlerDone <- true
		return nil
	})

	ss.TriggerShutdown(syscall.SIGTERM)

	// Wait should block until handler completes
	waitDone := make(chan bool, 1)
	go func() {
		ss.Wait()
		waitDone <- true
	}()

	// Verify handler completes first
	select {
	case <-handlerDone:
		// Good, handler completed
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Handler should have completed")
	}

	// Then wait should complete
	select {
	case <-waitDone:
		// Good, Wait() completed
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Wait() should have completed")
	}
}

func TestTimeoutBehavior(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: 100 * time.Millisecond,
	})

	handlerExecuted := false
	handlerTimedOut := false

	ss.RegisterHandler("slow", func(ctx context.Context) error {
		handlerExecuted = true
		// Wait longer than timeout
		select {
		case <-time.After(200 * time.Millisecond):
			handlerTimedOut = false
		case <-ctx.Done():
			handlerTimedOut = true
		}
		return nil
	})

	ss.TriggerShutdown(syscall.SIGTERM)
	ss.Wait()

	assert.True(t, handlerExecuted)
	assert.True(t, handlerTimedOut, "Handler should have been canceled due to timeout")
}

func TestMultipleSignals(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	executionCount := 0
	ss.RegisterHandler("test", func(ctx context.Context) error {
		executionCount++
		return nil
	})

	// Trigger shutdown multiple times
	ss.TriggerShutdown(syscall.SIGTERM)
	ss.TriggerShutdown(syscall.SIGINT)
	ss.TriggerShutdown(syscall.SIGTERM)
	ss.Wait()

	// Should only execute once due to sync.Once
	assert.Equal(t, 1, executionCount, "Handler should only execute once even with multiple signals")
}

func TestEmptyHandlers(t *testing.T) {
	completeCalled := false
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
		OnComplete: func(errors map[string]error) {
			completeCalled = true
			assert.Empty(t, errors)
		},
	})

	// No handlers registered
	ss.TriggerShutdown(syscall.SIGTERM)
	ss.Wait()

	assert.True(t, completeCalled)
}

func TestErrorPaths(t *testing.T) {
	errorMap := make(map[string]error)
	var errorMapMu sync.Mutex
	errorCallbacks := 0
	var errorCallbacksMu sync.Mutex

	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
		OnError: func(id string, err error) {
			errorCallbacksMu.Lock()
			errorCallbacks++
			errorCallbacksMu.Unlock()
			errorMapMu.Lock()
			errorMap[id] = err
			errorMapMu.Unlock()
		},
		OnComplete: func(errors map[string]error) {
			errorMapMu.Lock()
			errorMap = errors
			errorMapMu.Unlock()
		},
	})

	ss.RegisterHandler("error1", func(ctx context.Context) error {
		return errors.New("error one")
	})

	ss.RegisterHandler("success", func(ctx context.Context) error {
		return nil
	})

	ss.RegisterHandler("error2", func(ctx context.Context) error {
		return errors.New("error two")
	})

	ss.TriggerShutdown(syscall.SIGTERM)
	ss.Wait()

	// Should have 2 error callbacks
	errorCallbacksMu.Lock()
	callbackCount := errorCallbacks
	errorCallbacksMu.Unlock()
	assert.Equal(t, 2, callbackCount)

	// OnComplete should have all errors
	errorMapMu.Lock()
	err1 := errorMap["error1"]
	success := errorMap["success"]
	err2 := errorMap["error2"]
	errorMapMu.Unlock()

	assert.NotNil(t, err1)
	assert.Nil(t, success)
	assert.NotNil(t, err2)
}

func TestRegisterAfterShutdown(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	executed1 := false
	ss.RegisterHandler("before", func(ctx context.Context) error {
		executed1 = true
		return nil
	})

	ss.TriggerShutdown(syscall.SIGTERM)
	ss.Wait()

	// Try to register after shutdown
	executed2 := false
	ss.RegisterHandler("after", func(ctx context.Context) error {
		executed2 = true
		return nil
	})

	// Verify only first handler executed
	assert.True(t, executed1)
	assert.False(t, executed2)
	assert.Equal(t, 1, ss.GetHandlerCount(), "Handler registered after shutdown should not be added")
}

func TestUnregisterAfterShutdown(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	ss.RegisterHandler("test", func(ctx context.Context) error {
		return nil
	})

	ss.TriggerShutdown(syscall.SIGTERM)
	ss.Wait()

	// Try to unregister after shutdown
	removed := ss.UnregisterHandler("test")
	assert.False(t, removed, "Should not be able to unregister after shutdown")
}

func TestEmptyHandlerID(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	// Register with empty ID - should be silently ignored
	ss.RegisterHandler("", func(ctx context.Context) error {
		return nil
	})

	assert.Equal(t, 0, ss.GetHandlerCount())

	// Unregister with empty ID
	removed := ss.UnregisterHandler("")
	assert.False(t, removed)
}

func TestGetHandlerCount(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	assert.Equal(t, 0, ss.GetHandlerCount())

	ss.RegisterHandler("handler1", func(ctx context.Context) error {
		return nil
	})
	assert.Equal(t, 1, ss.GetHandlerCount())

	ss.RegisterHandler("handler2", func(ctx context.Context) error {
		return nil
	})
	assert.Equal(t, 2, ss.GetHandlerCount())

	ss.UnregisterHandler("handler1")
	assert.Equal(t, 1, ss.GetHandlerCount())
}

func TestContextCancellation(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: 50 * time.Millisecond,
	})

	ctxReceived := false
	ctxCanceled := false

	ss.RegisterHandler("test", func(ctx context.Context) error {
		ctxReceived = true
		// Wait for context to be canceled
		<-ctx.Done()
		ctxCanceled = true
		return ctx.Err()
	})

	ss.TriggerShutdown(syscall.SIGTERM)
	ss.Wait()

	assert.True(t, ctxReceived)
	assert.True(t, ctxCanceled)
}

func TestDefaultSignals(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	executed := false
	ss.RegisterHandler("test", func(ctx context.Context) error {
		executed = true
		return nil
	})

	// Should default to SIGTERM and SIGINT
	ss.TriggerShutdown(syscall.SIGTERM)
	ss.Wait()

	assert.True(t, executed)
}

func TestCustomSignals(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Signals: []os.Signal{syscall.SIGUSR1},
		Timeout: time.Minute,
	})

	executed := false
	ss.RegisterHandler("test", func(ctx context.Context) error {
		executed = true
		return nil
	})

	ss.TriggerShutdown(syscall.SIGUSR1)
	ss.Wait()

	assert.True(t, executed)
}

func TestBasicDependency(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	executionOrder := make([]string, 0)
	var orderMu sync.Mutex

	// Handler A has no dependencies
	ss.RegisterHandler("a", func(ctx context.Context) error {
		time.Sleep(50 * time.Millisecond) // Ensure A takes some time
		orderMu.Lock()
		executionOrder = append(executionOrder, "a")
		orderMu.Unlock()
		return nil
	})

	// Handler B depends on A
	err := ss.RegisterHandler("b", func(ctx context.Context) error {
		orderMu.Lock()
		executionOrder = append(executionOrder, "b")
		orderMu.Unlock()
		return nil
	}).DependsOn("a")

	require.NoError(t, err)

	ss.TriggerShutdown(syscall.SIGTERM)
	ss.Wait()

	// A should execute before B
	assert.Equal(t, []string{"a", "b"}, executionOrder)
}

func TestMultipleDependencies(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	executionOrder := make([]string, 0)
	var orderMu sync.Mutex

	ss.RegisterHandler("a", func(ctx context.Context) error {
		orderMu.Lock()
		executionOrder = append(executionOrder, "a")
		orderMu.Unlock()
		return nil
	})

	ss.RegisterHandler("b", func(ctx context.Context) error {
		orderMu.Lock()
		executionOrder = append(executionOrder, "b")
		orderMu.Unlock()
		return nil
	})

	// C depends on both A and B
	err := ss.RegisterHandler("c", func(ctx context.Context) error {
		orderMu.Lock()
		executionOrder = append(executionOrder, "c")
		orderMu.Unlock()
		return nil
	}).DependsOn("a", "b")

	require.NoError(t, err)

	ss.TriggerShutdown(syscall.SIGTERM)
	ss.Wait()

	// C should execute after both A and B
	assert.Contains(t, executionOrder[:2], "a")
	assert.Contains(t, executionOrder[:2], "b")
	assert.Equal(t, "c", executionOrder[2])
}

func TestMultipleHandlersDependOnSame(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	executionOrder := make([]string, 0)
	var orderMu sync.Mutex

	// A has no dependencies
	ss.RegisterHandler("a", func(ctx context.Context) error {
		time.Sleep(50 * time.Millisecond)
		orderMu.Lock()
		executionOrder = append(executionOrder, "a")
		orderMu.Unlock()
		return nil
	})

	// B and C both depend on A
	err1 := ss.RegisterHandler("b", func(ctx context.Context) error {
		orderMu.Lock()
		executionOrder = append(executionOrder, "b")
		orderMu.Unlock()
		return nil
	}).DependsOn("a")

	err2 := ss.RegisterHandler("c", func(ctx context.Context) error {
		orderMu.Lock()
		executionOrder = append(executionOrder, "c")
		orderMu.Unlock()
		return nil
	}).DependsOn("a")

	require.NoError(t, err1)
	require.NoError(t, err2)

	ss.TriggerShutdown(syscall.SIGTERM)
	ss.Wait()

	// A should be first
	assert.Equal(t, "a", executionOrder[0])
	// B and C should both execute after A (order between them is not guaranteed)
	assert.Contains(t, executionOrder[1:], "b")
	assert.Contains(t, executionOrder[1:], "c")
}

func TestChainedDependencies(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	executionOrder := make([]string, 0)
	var orderMu sync.Mutex

	ss.RegisterHandler("a", func(ctx context.Context) error {
		orderMu.Lock()
		executionOrder = append(executionOrder, "a")
		orderMu.Unlock()
		return nil
	})

	// B depends on A
	err1 := ss.RegisterHandler("b", func(ctx context.Context) error {
		orderMu.Lock()
		executionOrder = append(executionOrder, "b")
		orderMu.Unlock()
		return nil
	}).DependsOn("a")

	// C depends on B
	err2 := ss.RegisterHandler("c", func(ctx context.Context) error {
		orderMu.Lock()
		executionOrder = append(executionOrder, "c")
		orderMu.Unlock()
		return nil
	}).DependsOn("b")

	require.NoError(t, err1)
	require.NoError(t, err2)

	ss.TriggerShutdown(syscall.SIGTERM)
	ss.Wait()

	// Should execute in order: a, b, c
	assert.Equal(t, []string{"a", "b", "c"}, executionOrder)
}

func TestCycleDetection(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	ss.RegisterHandler("a", func(ctx context.Context) error {
		return nil
	})

	ss.RegisterHandler("b", func(ctx context.Context) error {
		return nil
	})

	// Create a cycle: a -> b -> a
	err1 := ss.RegisterHandler("a", func(ctx context.Context) error {
		return nil
	}).DependsOn("b")
	require.NoError(t, err1) // This should work (updating a)

	err2 := ss.RegisterHandler("b", func(ctx context.Context) error {
		return nil
	}).DependsOn("a")
	require.Error(t, err2)
	assert.Equal(t, ErrCycleDetected, err2)
}

func TestSelfDependencyError(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	ss.RegisterHandler("a", func(ctx context.Context) error {
		return nil
	})

	// Try to make a depend on itself
	err := ss.RegisterHandler("a", func(ctx context.Context) error {
		return nil
	}).DependsOn("a")

	require.Error(t, err)
	assert.Equal(t, ErrCycleDetected, errors.Unwrap(err))
}

func TestUnknownDependencyError(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	ss.RegisterHandler("a", func(ctx context.Context) error {
		return nil
	})

	// Try to depend on non-existent handler
	err := ss.RegisterHandler("b", func(ctx context.Context) error {
		return nil
	}).DependsOn("nonexistent")

	require.Error(t, err)
	assert.Equal(t, ErrUnknownDependency, errors.Unwrap(err))
}

func TestDependencyOnFailedHandler(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	executed := make(map[string]bool)

	// A will fail
	ss.RegisterHandler("a", func(ctx context.Context) error {
		executed["a"] = true
		return errors.New("a failed")
	})

	// B depends on A, but A fails, so B should not execute
	err := ss.RegisterHandler("b", func(ctx context.Context) error {
		executed["b"] = true
		return nil
	}).DependsOn("a")

	require.NoError(t, err)

	ss.TriggerShutdown(syscall.SIGTERM)
	ss.Wait()

	// A should execute and fail
	assert.True(t, executed["a"])
	// B should NOT execute because A failed
	assert.False(t, executed["b"])
}

func TestMultipleDependsOnCalls(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	executionOrder := make([]string, 0)
	var orderMu sync.Mutex

	ss.RegisterHandler("a", func(ctx context.Context) error {
		orderMu.Lock()
		executionOrder = append(executionOrder, "a")
		orderMu.Unlock()
		return nil
	})

	ss.RegisterHandler("b", func(ctx context.Context) error {
		orderMu.Lock()
		executionOrder = append(executionOrder, "b")
		orderMu.Unlock()
		return nil
	})

	// Add dependencies in multiple calls
	reg := ss.RegisterHandler("c", func(ctx context.Context) error {
		orderMu.Lock()
		executionOrder = append(executionOrder, "c")
		orderMu.Unlock()
		return nil
	})

	err1 := reg.DependsOn("a")
	require.NoError(t, err1)

	err2 := reg.DependsOn("b")
	require.NoError(t, err2)

	ss.TriggerShutdown(syscall.SIGTERM)
	ss.Wait()

	// C should execute after both A and B
	assert.Contains(t, executionOrder[:2], "a")
	assert.Contains(t, executionOrder[:2], "b")
	assert.Equal(t, "c", executionOrder[2])
}

func TestComplexDependencyGraph(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	executionOrder := make([]string, 0)
	var orderMu sync.Mutex

	// Create a complex graph:
	//   a -> b -> d
	//   a -> c -> d
	//   d -> e

	ss.RegisterHandler("a", func(ctx context.Context) error {
		orderMu.Lock()
		executionOrder = append(executionOrder, "a")
		orderMu.Unlock()
		return nil
	})

	ss.RegisterHandler("b", func(ctx context.Context) error {
		orderMu.Lock()
		executionOrder = append(executionOrder, "b")
		orderMu.Unlock()
		return nil
	}).DependsOn("a")

	ss.RegisterHandler("c", func(ctx context.Context) error {
		orderMu.Lock()
		executionOrder = append(executionOrder, "c")
		orderMu.Unlock()
		return nil
	}).DependsOn("a")

	ss.RegisterHandler("d", func(ctx context.Context) error {
		orderMu.Lock()
		executionOrder = append(executionOrder, "d")
		orderMu.Unlock()
		return nil
	}).DependsOn("b", "c")

	ss.RegisterHandler("e", func(ctx context.Context) error {
		orderMu.Lock()
		executionOrder = append(executionOrder, "e")
		orderMu.Unlock()
		return nil
	}).DependsOn("d")

	ss.TriggerShutdown(syscall.SIGTERM)
	ss.Wait()

	// Verify order constraints
	aIdx := -1
	bIdx := -1
	cIdx := -1
	dIdx := -1
	eIdx := -1

	for i, id := range executionOrder {
		switch id {
		case "a":
			aIdx = i
		case "b":
			bIdx = i
		case "c":
			cIdx = i
		case "d":
			dIdx = i
		case "e":
			eIdx = i
		}
	}

	// A should be first
	assert.Equal(t, 0, aIdx)
	// B and C should come after A
	assert.Greater(t, bIdx, aIdx)
	assert.Greater(t, cIdx, aIdx)
	// D should come after both B and C
	assert.Greater(t, dIdx, bIdx)
	assert.Greater(t, dIdx, cIdx)
	// E should come after D
	assert.Greater(t, eIdx, dIdx)
}

func TestEmptyDependencyID(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	ss.RegisterHandler("a", func(ctx context.Context) error {
		return nil
	})

	// Try to depend on empty ID
	err := ss.RegisterHandler("b", func(ctx context.Context) error {
		return nil
	}).DependsOn("")

	require.Error(t, err)
	assert.Equal(t, ErrUnknownDependency, errors.Unwrap(err))
}

func TestUnregisterRemovesDependencies(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	executed := make(map[string]bool)
	var executedMu sync.Mutex

	ss.RegisterHandler("a", func(ctx context.Context) error {
		executedMu.Lock()
		executed["a"] = true
		executedMu.Unlock()
		return nil
	})

	ss.RegisterHandler("b", func(ctx context.Context) error {
		executedMu.Lock()
		executed["b"] = true
		executedMu.Unlock()
		return nil
	}).DependsOn("a")

	ss.RegisterHandler("c", func(ctx context.Context) error {
		executedMu.Lock()
		executed["c"] = true
		executedMu.Unlock()
		return nil
	}).DependsOn("a")

	// Unregister A
	removed := ss.UnregisterHandler("a")
	assert.True(t, removed)

	// Re-register B and C without dependencies (they should work now)
	ss.RegisterHandler("b", func(ctx context.Context) error {
		executedMu.Lock()
		executed["b"] = true
		executedMu.Unlock()
		return nil
	})

	ss.RegisterHandler("c", func(ctx context.Context) error {
		executedMu.Lock()
		executed["c"] = true
		executedMu.Unlock()
		return nil
	})

	ss.TriggerShutdown(syscall.SIGTERM)
	ss.Wait()

	// B and C should execute (they no longer depend on A)
	executedMu.Lock()
	bExecuted := executed["b"]
	cExecuted := executed["c"]
	executedMu.Unlock()

	assert.True(t, bExecuted)
	assert.True(t, cExecuted)
}

func TestUnregisterRemovesDependentHandlers(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	// Create dependency chain: a -> b -> c
	ss.RegisterHandler("a", func(ctx context.Context) error {
		return nil
	})

	ss.RegisterHandler("b", func(ctx context.Context) error {
		return nil
	}).DependsOn("a")

	ss.RegisterHandler("c", func(ctx context.Context) error {
		return nil
	}).DependsOn("b")

	// Also create d that depends on a
	ss.RegisterHandler("d", func(ctx context.Context) error {
		return nil
	}).DependsOn("a")

	// Verify all handlers are registered
	assert.Equal(t, 4, ss.GetHandlerCount())

	// Unregister A - should remove B, C, and D (all depend on A, directly or indirectly)
	removed := ss.UnregisterHandler("a")
	assert.True(t, removed)

	// All handlers should be removed
	assert.Equal(t, 0, ss.GetHandlerCount())
}

func TestUnregisterRemovesTransitiveDependents(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	// Create: a -> b -> c -> d
	ss.RegisterHandler("a", func(ctx context.Context) error {
		return nil
	})

	ss.RegisterHandler("b", func(ctx context.Context) error {
		return nil
	}).DependsOn("a")

	ss.RegisterHandler("c", func(ctx context.Context) error {
		return nil
	}).DependsOn("b")

	ss.RegisterHandler("d", func(ctx context.Context) error {
		return nil
	}).DependsOn("c")

	// Also create e that doesn't depend on anything
	ss.RegisterHandler("e", func(ctx context.Context) error {
		return nil
	})

	assert.Equal(t, 5, ss.GetHandlerCount())

	// Unregister A - should remove B, C, and D (transitive dependents), but not E
	removed := ss.UnregisterHandler("a")
	assert.True(t, removed)

	// Only E should remain
	assert.Equal(t, 1, ss.GetHandlerCount())

	// Verify E is still registered
	executed := false
	ss.RegisterHandler("e", func(ctx context.Context) error {
		executed = true
		return nil
	})

	ss.TriggerShutdown(syscall.SIGTERM)
	ss.Wait()

	assert.True(t, executed)
}

func TestUnregisterMultipleDependents(t *testing.T) {
	ss := NewSafeStop(&Opts{
		Timeout: time.Minute,
	})

	// Create: a -> b, a -> c, a -> d
	ss.RegisterHandler("a", func(ctx context.Context) error {
		return nil
	})

	ss.RegisterHandler("b", func(ctx context.Context) error {
		return nil
	}).DependsOn("a")

	ss.RegisterHandler("c", func(ctx context.Context) error {
		return nil
	}).DependsOn("a")

	ss.RegisterHandler("d", func(ctx context.Context) error {
		return nil
	}).DependsOn("a")

	// Create e that doesn't depend on a
	ss.RegisterHandler("e", func(ctx context.Context) error {
		return nil
	})

	assert.Equal(t, 5, ss.GetHandlerCount())

	// Unregister A - should remove B, C, and D, but not E
	removed := ss.UnregisterHandler("a")
	assert.True(t, removed)

	// Only E should remain
	assert.Equal(t, 1, ss.GetHandlerCount())
}
