package main

import "time"

func r4ResetLoop() { throttledTo, throttleAt = 0, time.Time{} }
func r4AllowStep() { throttleAt = time.Time{} }
