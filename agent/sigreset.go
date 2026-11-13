package main

import (
	"os"
	"os/signal"
	"syscall"
)

// The boot script starts the panel in the background ("… &"), and a shell starts background programs with
// SIGINT and SIGQUIT ignored. Ignored signals are passed on to every program started from here, so in an SSH
// session Ctrl+C did nothing: a "sleep" or a loop couldn't be interrupted. Catching them here (and doing
// nothing with them) means programs started from the panel get them back at their default: Ctrl+C works.
// The panel itself still isn't stopped by them, as before.
func init() {
	c := make(chan os.Signal, 4)
	signal.Notify(c, syscall.SIGINT, syscall.SIGQUIT)
	go func() {
		for range c {
		}
	}()
}
