package main

func r4ResetUnlock() { fanLockMu.Lock(); fanUnlockAt = 0; fanLockMu.Unlock() }
