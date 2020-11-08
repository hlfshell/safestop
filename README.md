# SafeStop
`SafeStop` is a golang daemon for large services that will coordinate safely shutting down multiple branches/background processes when a SIGINT/SIGTERM is executed.

For each service, create a function to safely wind or shut down the service - IE stop accepting new requests, kill timed-delayed daemons, or simply pause and wait until existing actions complete to avoid data loss. Instantiate a SafeStop struct and register these functions to it.

When a specified signal is received, SafeStop will begin executing each registered handler function. SafeStop also has a specified timeout as well.

This is an initial first-pass at an idea.