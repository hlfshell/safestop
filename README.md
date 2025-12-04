# SafeStop

## What is it?
`SafeStop` is a golang library for large applications that will coordinate safely shutting down multiple services/background processes when an OS signal (such as `SIGINT`/`SIGTERM`, which are the default signals handled) is executed.

## Installation

```
go get github.com/hlfshell/safestop
```

## How to use SafeStop

### Basic Usage

```go
ss := safestop.NewSafeStop(&safestop.Opts{
    Signals: []os.Signal{syscall.SIGINT, syscall.SIGTERM},
    Timeout: time.Minute,
    OnComplete: nil,
    OnError: nil,
})

ss.RegisterHandler("handler-id", shutdownFunction)
```

For each service, create a function to safely wind or shut down the service - IE stop accepting new requests, kill timed-delayed daemons, or simply pause and wait until existing actions complete to avoid data loss. Instantiate a SafeStop struct and register these functions to it.

Each of these functions must meet the type `HaltHandler`, which is of expected type `func(ctx context.Context) error`. The context being passed in will be a timeout context with the specified timeout within the options. In the example above, `shutdownFunction` is assumed to be a `HaltHandler`.

### Handler Execution

When a specified signal is received, SafeStop will begin executing each registered `HaltHandler`. By default, handlers execute concurrently. However, you can specify dependencies to control execution order.

**Note:** Handlers without dependencies execute concurrently. Handlers with dependencies execute only after their dependencies complete successfully (without error).

### Callbacks

Create an `OnComplete` function of the type `func(errors map[string]error)` to handle the `SafeStop` service completing the calls to each `HaltHandler`. Note that if the handlers error out, their given `id` at registry time will be paired with an error within the map. A handler that did not return an error will have `nil` within its field.

The `OnError` callback of type `func(identifier string, err error)` is called immediately when any handler encounters an error during execution.

### Example

```go
package main

import (
    "context"
    "fmt"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/hlfshell/safestop"
)

func main() {
    ss := safestop.NewSafeStop(&safestop.Opts{
        Signals: []os.Signal{syscall.SIGINT, syscall.SIGTERM},
        Timeout: 30 * time.Second,
        OnError: func(id string, err error) {
            fmt.Printf("Handler %s failed: %v\n", id, err)
        },
        OnComplete: func(errors map[string]error) {
            fmt.Println("All handlers completed")
            for id, err := range errors {
                if err != nil {
                    fmt.Printf("  %s: %v\n", id, err)
                }
            }
        },
    })

    // Register shutdown handlers
    ss.RegisterHandler("database", func(ctx context.Context) error {
        // Close database connections gracefully
        return db.Close()
    })

    ss.RegisterHandler("http-server", func(ctx context.Context) error {
        // Stop accepting new connections and wait for existing to finish
        return httpServer.Shutdown(ctx)
    })

    ss.RegisterHandler("background-worker", func(ctx context.Context) error {
        // Stop background worker
        worker.Stop()
        return nil
    })

    // Wait for shutdown signal
    ss.Wait()
}
```

### New Features

#### Manual Shutdown

You can manually trigger shutdown without waiting for a signal:

```go
ss.Shutdown()  // Triggers shutdown immediately
ss.Wait()      // Wait for all handlers to complete
```

#### Unregister Handlers

Remove handlers dynamically:

```go
removed := ss.UnregisterHandler("handler-id")
if removed {
    fmt.Println("Handler removed")
}
```

**Important:** When you unregister a handler, all handlers that depend on it (directly or indirectly) are also automatically removed. This is because those handlers can no longer function properly without their dependency.

For example, if you have:
- Handler A (no dependencies)
- Handler B depends on A
- Handler C depends on B

Unregistering A will remove A, B, and C (since B depends on A, and C depends on B which depends on A).

If multiple handlers depend on the same handler, they are all removed when that handler is unregistered.

#### Get Handler Count

Check how many handlers are registered:

```go
count := ss.GetHandlerCount()
fmt.Printf("Registered handlers: %d\n", count)
```

#### Handler Dependencies

You can specify that a handler should only execute after other handlers complete successfully:

```go
// Register handler A
ss.RegisterHandler("a", func(ctx context.Context) error {
    // Close database connections
    return db.Close()
})

// Register handler B that depends on A
err := ss.RegisterHandler("b", func(ctx context.Context) error {
    // This will only run after A completes successfully
    return cleanup()
}).DependsOn("a")

if err != nil {
    // Handle error (e.g., cycle detected, unknown dependency)
    log.Fatal(err)
}
```

**Key behaviors:**
- If a dependency fails (returns an error), dependent handlers will **not** execute
- Multiple handlers can depend on the same handler - they all execute after it completes concurrently
- Dependencies can be chained: A -> B -> C
- Multiple dependencies are supported: `DependsOn("a", "b", "c")`
- You can call `DependsOn` multiple times to add dependencies incrementally
- **Cycle detection**: If a dependency cycle is detected, registration returns an error
- **Unknown dependency**: If a dependency ID doesn't exist, registration returns an error

**Example with complex dependencies:**

```go
// Database must close before cache and logger
ss.RegisterHandler("database", func(ctx context.Context) error {
    return db.Close()
})

// Cache depends on database
ss.RegisterHandler("cache", func(ctx context.Context) error {
    return cache.Flush()
}).DependsOn("database")

// Logger depends on database
ss.RegisterHandler("logger", func(ctx context.Context) error {
    return logger.Close()
}).DependsOn("database")

// Final cleanup depends on both cache and logger
ss.RegisterHandler("cleanup", func(ctx context.Context) error {
    return finalCleanup()
}).DependsOn("cache", "logger")
```

**Error handling:**

```go
err := ss.RegisterHandler("b", handler).DependsOn("a")
if err != nil {
    if errors.Is(err, safestop.ErrCycleDetected) {
        log.Fatal("Dependency cycle detected!")
    }
    if errors.Is(err, safestop.ErrUnknownDependency) {
        log.Fatal("Unknown dependency!")
    }
}
```

#### Nil Options

SafeStop handles nil options gracefully, using defaults:

```go
ss := safestop.NewSafeStop(nil)  // Uses default signals and timeout
```

### Testing

For testing purposes, you can use `TriggerShutdown` to simulate a signal:

```go
ss := safestop.NewSafeStop(&safestop.Opts{
    Timeout: time.Minute,
})

ss.RegisterHandler("test", func(ctx context.Context) error {
    // Test handler
    return nil
})

// Trigger shutdown for testing
ss.TriggerShutdown(syscall.SIGTERM)
ss.Wait()
```

## Requirements

- Go 1.21 or later
