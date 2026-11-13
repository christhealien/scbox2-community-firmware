package main

import (
	"testing"
	"time"
)

func TestBoardHealthScan(t *testing.T) {
	bhMu.Lock()
	bhBoards = map[int]*bhBoard{}
	bhSeen = map[string]bhSeenRec{}
	bhMu.Unlock()
	ts := time.Now().Add(-10 * time.Minute).Format("2006-01-02 15:04:05")
	old := time.Now().Add(-2 * time.Hour).Format("2006-01-02 15:04:05")
	scanBoardHealth([]string{
		" [" + ts + "] C0: SCLITE Init sucessed. 46 chips, 3680 Total goodcores.",
		" [" + ts + "] C1: SCLITE Init sucessed. 44 chips, 3500 Total goodcores.",
		" [" + ts + "] C1: impossible thing happened!!",
		" [" + ts + "] C1: impossible thing happened!!",
		" [" + old + "] C3: impossible thing happened!!",
		" [" + ts + "] C2: !!!SEND JOB FAILD 10 TIMES, REINIT THIS CPB!!!",
		" [" + ts + "] C2: Read Nonce 5  Faild 10 Times, Reinit Device!!!",
		" [" + ts + "] C3: Auto addressing failed",
		" [" + ts + "] C3: Init failed 5 Times, Try reboot later!!!",
		" [" + time.Now().Format("2006-01-02 15:04:05") + "] C0: Chip Avgtemp 61.000000'C, MaxTemp 66.500000'C, CpbTemp 58.875000'C",
		" [" + ts + "] C0: nonce#(5/5) 0x1 ntime#0 from 1.2  BAD(1/2)",
	})
	// the same lines read again (log re-read whole) must not count twice
	scanBoardHealth([]string{" [" + ts + "] C1: impossible thing happened!!", " [" + ts + "] C1: impossible thing happened!!"})
	got := map[int]boardHealth{}
	for _, h := range boardHealthView() {
		got[h.ID] = h
	}
	if got[0].Chips != 46 || got[0].MaxTemp != 66.5 || got[0].Dropped24 != 0 {
		t.Errorf("board 0 %+v", got[0])
	}
	if got[1].Chips != 44 || got[1].Dropped1h != 2 || got[1].Dropped24 != 2 {
		t.Errorf("board 1 %+v", got[1])
	}
	if got[2].Reinit1h != 2 || got[2].Chips != -1 {
		t.Errorf("board 2 %+v", got[2])
	}
	if got[3].Dropped1h != 0 || got[3].Dropped24 != 1 || got[3].AddrFail24 != 1 || got[3].InitFail24 != 1 {
		t.Errorf("board 3 %+v", got[3])
	}
}
