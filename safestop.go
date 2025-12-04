package safestop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// HaltHandler - a handler function that accepts a timed context and returns an error (or nil) if
// it successfully stops from signal.
type HaltHandler func(ctx context.Context) error

// ErrUnknownDependency is returned when a dependency references a handler that doesn't exist.
var ErrUnknownDependency = errors.New("unknown dependency: handler not registered")

// ErrCycleDetected is returned when a dependency cycle is detected.
var ErrCycleDetected = errors.New("dependency cycle detected")

// SafeStop is the core of the package - it handles registering and properly triggering registered
// HaltHandler functions simultaneously upon a registered kill signal coming through.
// HaltHandler functions will use contexts that may be canceled due to timeouts.
// Handlers can have dependencies - if handler B depends on handler A, B will only execute
// after A completes successfully (without error). Multiple handlers can depend on the same handler.
type SafeStop struct {
	fncs map[string]HaltHandler // collection of HaltHandlers Safestop will call on a signal

	// dependencies maps handler ID to its list of dependency IDs
	dependencies map[string][]string

	lock sync.RWMutex // We need to ensure that we have atomic access to fncs and dependencies

	signalsChannel chan os.Signal

	options *Opts

	// shutdownOnce ensures shutdown only happens once
	shutdownOnce sync.Once

	// shutdownState tracks if shutdown has been triggered (0 = not shutdown, 1 = shutdown)
	shutdownState int32

	// done channel is closed when shutdown completes
	done chan struct{}
}

// HandlerRegistration is returned by RegisterHandler to allow chaining dependency declarations.
type HandlerRegistration struct {
	ss         *SafeStop
	id         string
	handler    HaltHandler
	deps       []string
	registered bool
}

// Opts is the config for a given SafeStop
type Opts struct {
	Signals []os.Signal //defaults to SIGINT and SIGTERM
	Timeout time.Duration

	//OnError is an optional function that is called whenever
	//any handler encounters an error. It will consist of the
	//given identifer for the handler (passed at registry
	//time) and the error generated.
	OnError func(identifier string, err error)

	//OnComplete is an optional function called when all
	//HaltHandlers are called. It will pass a map of each
	//registered handler and whether it errored out.
	OnComplete func(errors map[string]error)
}

// NewSafeStop instantiates a new SafeStop as per options.
// If opts is nil, default options will be used (SIGINT/SIGTERM, 1 minute timeout).
func NewSafeStop(opts *Opts) *SafeStop {
	// Handle nil opts
	if opts == nil {
		opts = &Opts{}
	}

	ss := &SafeStop{
		fncs:           make(map[string]HaltHandler),
		dependencies:   make(map[string][]string),
		signalsChannel: make(chan os.Signal, 1), // Buffered to prevent blocking
		options:        opts,
		done:           make(chan struct{}),
	}

	//Default to SIGINT and SIGTERM
	if len(ss.options.Signals) == 0 {
		ss.options.Signals = []os.Signal{syscall.SIGTERM, syscall.SIGINT}
	}

	//Await the signals to come in - this will
	//hold forever until a registered signal occurs
	go ss.awaitSignal()

	//Now that we can handle incoming signals, register for them
	ss.registerSignals()

	return ss
}

// awaitSignal is the function that handles the triggering from an
// incoming signal. It is called within the creation of a SafeStop
// and should not be called again or you will double Handler execs
func (ss *SafeStop) awaitSignal() {
	<-ss.signalsChannel
	ss.executeShutdown()
}

// executeShutdown executes all registered handlers respecting their dependencies.
// Handlers with no dependencies execute first. Handlers that depend on others only
// execute after their dependencies complete successfully (without error).
func (ss *SafeStop) executeShutdown() {
	ss.shutdownOnce.Do(func() {
		atomic.StoreInt32(&ss.shutdownState, 1)

		// Copy handlers and dependencies to avoid holding lock during execution
		ss.lock.RLock()
		handlers := make(map[string]HaltHandler, len(ss.fncs))
		dependencies := make(map[string][]string, len(ss.dependencies))
		for id, handler := range ss.fncs {
			handlers[id] = handler
		}
		for id, deps := range ss.dependencies {
			depsCopy := make([]string, len(deps))
			copy(depsCopy, deps)
			dependencies[id] = depsCopy
		}
		ss.lock.RUnlock()

		// Setup a timeout context - all functions should finish within a set time
		ctx, cancelFunc := context.WithTimeout(context.Background(), ss.options.Timeout)
		defer cancelFunc()

		errors := make(map[string]error, len(handlers))
		var errorsMu sync.Mutex

		// Track which handlers have completed successfully
		completed := make(map[string]bool)
		var completedMu sync.Mutex

		// Execute handlers in dependency order
		remaining := make(map[string]bool)
		for id := range handlers {
			remaining[id] = true
		}

		for len(remaining) > 0 {
			// Find handlers ready to execute (no dependencies or all dependencies completed successfully)
			ready := make([]string, 0)
			blocked := make([]string, 0)
			completedMu.Lock()
			errorsMu.Lock()
			for id := range remaining {
				deps := dependencies[id]
				canRun := true
				hasFailedDep := false
				for _, dep := range deps {
					// Check if dependency failed
					if err, exists := errors[dep]; exists && err != nil {
						// Dependency failed - this handler should never run
						hasFailedDep = true
						canRun = false
						break
					}
					// Check if dependency completed successfully
					if !completed[dep] {
						// Dependency hasn't completed yet (might still be running or not started)
						canRun = false
						break
					}
				}
				if canRun {
					ready = append(ready, id)
				} else if hasFailedDep {
					// Handler has a failed dependency - remove it from remaining, it will never run
					blocked = append(blocked, id)
				}
			}
			errorsMu.Unlock()
			completedMu.Unlock()

			// Remove blocked handlers from remaining
			for _, id := range blocked {
				delete(remaining, id)
				// Mark as not executed (nil error means it didn't run due to failed dependency)
				errorsMu.Lock()
				errors[id] = nil // nil means it didn't execute due to failed dependency
				errorsMu.Unlock()
			}

			if len(ready) == 0 {
				// No handlers are ready - check if we're stuck (all remaining have failed dependencies)
				// or if we're done
				if len(blocked) == 0 && len(remaining) > 0 {
					// This shouldn't happen with valid dependency graphs, but handle gracefully
					// by executing remaining handlers (they might have no dependencies)
					for id := range remaining {
						ready = append(ready, id)
					}
				}
			}

			// Execute ready handlers concurrently
			var batchWg sync.WaitGroup
			for _, id := range ready {
				delete(remaining, id)
				batchWg.Add(1)
				go func(handlerID string, h HaltHandler) {
					defer batchWg.Done()
					err := h(ctx)
					errorsMu.Lock()
					errors[handlerID] = err
					errorsMu.Unlock()

					// Only mark as completed if no error
					if err == nil {
						completedMu.Lock()
						completed[handlerID] = true
						completedMu.Unlock()
					}

					if err != nil && ss.options.OnError != nil {
						ss.options.OnError(handlerID, err)
					}
				}(id, handlers[id])
			}

			// Wait for this batch to complete before checking for next batch
			batchWg.Wait()
		}

		if ss.options.OnComplete != nil {
			ss.options.OnComplete(errors)
		}

		// Signal that shutdown is complete
		close(ss.done)
	})
}

// RegisterHandler will add the given HaltHandler for SafeStop to manage.
// If a handler with the same id already exists, it will be overwritten.
// Registering a handler after shutdown has been triggered will have no effect.
// Returns a HandlerRegistration builder that allows chaining DependsOn calls.
func (ss *SafeStop) RegisterHandler(id string, fnc HaltHandler) *HandlerRegistration {
	if id == "" {
		// Return a registration that will fail on DependsOn
		return &HandlerRegistration{
			ss:         ss,
			id:         "",
			handler:    fnc,
			registered: false,
		}
	}

	// Check if shutdown has already started
	if atomic.LoadInt32(&ss.shutdownState) == 1 {
		return &HandlerRegistration{
			ss:         ss,
			id:         id,
			handler:    fnc,
			registered: false,
		}
	}

	ss.lock.Lock()
	ss.fncs[id] = fnc
	// Initialize empty dependencies if not exists
	if _, exists := ss.dependencies[id]; !exists {
		ss.dependencies[id] = []string{}
	}
	ss.lock.Unlock()

	return &HandlerRegistration{
		ss:         ss,
		id:         id,
		handler:    fnc,
		deps:       []string{},
		registered: true,
	}
}

// DependsOn adds dependencies to a handler. The handler will only execute after
// all its dependencies complete successfully (without error).
// Returns an error if:
//   - The handler ID is empty or invalid
//   - Any dependency ID doesn't exist
//   - Adding the dependency would create a cycle
//
// Can be called multiple times to add multiple dependencies.
func (hr *HandlerRegistration) DependsOn(depIDs ...string) error {
	if !hr.registered {
		if hr.id == "" {
			return errors.New("cannot add dependencies: handler ID is empty")
		}
		return errors.New("cannot add dependencies: handler not registered (shutdown may have started)")
	}

	if len(depIDs) == 0 {
		return nil
	}

	hr.ss.lock.Lock()
	defer hr.ss.lock.Unlock()

	// Check if all dependencies exist
	for _, depID := range depIDs {
		if depID == "" {
			return fmt.Errorf("%w: empty dependency ID", ErrUnknownDependency)
		}
		if _, exists := hr.ss.fncs[depID]; !exists {
			return fmt.Errorf("%w: '%s'", ErrUnknownDependency, depID)
		}
		// Don't allow self-dependency
		if depID == hr.id {
			return fmt.Errorf("%w: handler cannot depend on itself", ErrCycleDetected)
		}
	}

	// Add dependencies temporarily to check for cycles
	newDeps := make([]string, len(hr.ss.dependencies[hr.id]))
	copy(newDeps, hr.ss.dependencies[hr.id])
	newDeps = append(newDeps, depIDs...)

	// Check for cycles
	if err := hr.ss.detectCycle(hr.id, newDeps); err != nil {
		return err
	}

	// All checks passed, add dependencies
	hr.ss.dependencies[hr.id] = newDeps
	hr.deps = newDeps

	return nil
}

// detectCycle performs a DFS to detect cycles in the dependency graph.
func (ss *SafeStop) detectCycle(handlerID string, newDeps []string) error {
	visited := make(map[string]bool)
	recStack := make(map[string]bool)

	var dfs func(id string) bool
	dfs = func(id string) bool {
		visited[id] = true
		recStack[id] = true

		// Get dependencies for this handler
		var deps []string
		if id == handlerID {
			// Use the new dependencies being added
			deps = newDeps
		} else {
			deps = ss.dependencies[id]
		}

		// Check all dependencies
		for _, dep := range deps {
			if !visited[dep] {
				if dfs(dep) {
					return true // Cycle found
				}
			} else if recStack[dep] {
				return true // Back edge found, cycle detected
			}
		}

		recStack[id] = false
		return false
	}

	if dfs(handlerID) {
		return ErrCycleDetected
	}
	return nil
}

// UnregisterHandler removes a handler by its ID.
// Also removes the handler from dependency lists and removes its own dependencies.
// Importantly, any handlers that depend on the removed handler are also removed
// (recursively - if handler B depends on A and handler C depends on B, removing A
// will remove both B and C).
// Returns true if the handler was found and removed, false otherwise.
func (ss *SafeStop) UnregisterHandler(id string) bool {
	if id == "" {
		return false
	}

	// Check if shutdown has already started
	if atomic.LoadInt32(&ss.shutdownState) == 1 {
		return false
	}

	ss.lock.Lock()
	defer ss.lock.Unlock()

	_, exists := ss.fncs[id]
	if !exists {
		return false
	}

	// Find all handlers that depend on this handler (directly or indirectly)
	// and remove them recursively
	toRemove := ss.findDependentHandlers(id)
	toRemove[id] = true // Include the handler itself

	// Remove all identified handlers
	for handlerID := range toRemove {
		delete(ss.fncs, handlerID)
		delete(ss.dependencies, handlerID)
	}

	// Remove all removed handlers from remaining handlers' dependency lists
	for handlerID := range toRemove {
		for otherID, deps := range ss.dependencies {
			newDeps := make([]string, 0, len(deps))
			for _, dep := range deps {
				if dep != handlerID {
					newDeps = append(newDeps, dep)
				}
			}
			ss.dependencies[otherID] = newDeps
		}
	}

	return true
}

// findDependentHandlers finds all handlers that depend on the given handler,
// either directly or indirectly (transitively).
func (ss *SafeStop) findDependentHandlers(handlerID string) map[string]bool {
	dependent := make(map[string]bool)

	// Use DFS to find all handlers that depend on handlerID
	var dfs func(id string)
	dfs = func(id string) {
		// Find all handlers that directly depend on id
		for otherID, deps := range ss.dependencies {
			if dependent[otherID] {
				continue // Already processed
			}
			for _, dep := range deps {
				if dep == id {
					dependent[otherID] = true
					// Recursively find handlers that depend on this one
					dfs(otherID)
					break
				}
			}
		}
	}

	dfs(handlerID)
	return dependent
}

// Shutdown manually triggers shutdown without waiting for a signal.
// This is useful for programmatic shutdown scenarios.
// It is safe to call multiple times - only the first call will execute shutdown.
func (ss *SafeStop) Shutdown() {
	select {
	case ss.signalsChannel <- syscall.SIGTERM:
	default:
		// Channel might be closed or full, trigger directly
		ss.executeShutdown()
	}
}

// Wait blocks until shutdown completes. This is useful for ensuring
// all handlers have finished before the program exits.
func (ss *SafeStop) Wait() {
	<-ss.done
}

// GetHandlerCount returns the number of currently registered handlers.
// Useful for testing and monitoring.
func (ss *SafeStop) GetHandlerCount() int {
	ss.lock.RLock()
	defer ss.lock.RUnlock()
	return len(ss.fncs)
}

// TriggerShutdown is a test helper that manually triggers shutdown.
// It sends a signal to the internal channel to simulate an OS signal.
// This method is intended for testing purposes.
func (ss *SafeStop) TriggerShutdown(sig os.Signal) {
	select {
	case ss.signalsChannel <- sig:
	default:
		// If channel is full or closed, trigger directly
		ss.executeShutdown()
	}
}

// registerSignals internally manages registering for each signal requested
// and then calls the registered functions upon trigger
func (ss *SafeStop) registerSignals() {
	signal.Notify(ss.signalsChannel, ss.options.Signals...)
}
