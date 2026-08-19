package state

import (
	"testing"
	"time"
)

func TestChooseNext(t *testing.T) {
	t0 := time.Now()
	older := t0.Add(-time.Hour)

	if p := ChooseNext(nil); p != nil {
		t.Errorf("empty list: got %v, want nil", p)
	}
	if p := ChooseNext([]*Pane{{PaneID: "%1", Status: StatusWorking}}); p != nil {
		t.Errorf("only working agents: got %v, want nil", p)
	}

	panes := []*Pane{
		{PaneID: "%1", Status: StatusDone, StatusSince: older},
		{PaneID: "%2", Status: StatusWaitingPermission, StatusSince: t0},
		{PaneID: "%3", Status: StatusWaitingInput, StatusSince: older},
		{PaneID: "%4", Status: StatusWorking, StatusSince: older},
	}
	if p := ChooseNext(panes); p.PaneID != "%2" {
		t.Errorf("permission must win over done/idle, got %s", p.PaneID)
	}

	twoDone := []*Pane{
		{PaneID: "%5", Status: StatusDone, StatusSince: t0},
		{PaneID: "%6", Status: StatusDone, StatusSince: older},
	}
	if p := ChooseNext(twoDone); p.PaneID != "%6" {
		t.Errorf("longest-waiting must win within a status, got %s", p.PaneID)
	}
}
