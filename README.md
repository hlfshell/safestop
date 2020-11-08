# SafeStop

## What is it?
`SafeStop` is a golang daemon for large applications that will coordinate safely shutting down multiple services/background processes when an OS signal (such as `SIGINT`/`SIGTERM`, which are the default signals handled) is executed.

## Installation

```
go get github.com/hlfshell/safestop
```

## How to use SafeStop

```
ss := safestop.NewSafeStop(&Opts{
    Signals: []os.Signal{syscall.SIGINT, syscall.SIGTEMR},
    Timeout: time.Minute,
    OnComplete: nil,
    OnError: nil,
})

ss.RegisterHandler("handler-id", shutdownFunction)
```

For each service, create a function to safely wind or shut down the service - IE stop accepting new requests, kill timed-delayed daemons, or simply pause and wait until existing actions complete to avoid data loss. Instantiate a SafeStop struct and register these functions to it.

Each of these functions must meet the type `HaltHandler`, which is of expected type `func(ctx context.Context) error`. The context being passed in will be a timeout context with the specified timeout within the options. In the example above, `shutdownFunction` is assumed to be a `HaltHandler`.

Create a `OnComplete` function of the type `func(errors map[string]error)` to handle the `SafeStop` service from completeing the calls to each `HaltHandler`. Note that if the handlers errors out, their given `id` at registry time will be paired with an error within the map. A handler that did not return an error will have `nil` within its field.

When a specified signal is received, SafeStop will begin executing each registered `HaltHandler` simultaneously.