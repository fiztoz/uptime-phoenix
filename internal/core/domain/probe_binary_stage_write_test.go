package domain

import (
	"path"
	"strings"
	"testing"
)

func TestProbeBinaryStageSeal(t *testing.T) {
	intent := binaryStageDomainIntent()
	seal := ProbeBinaryStageSeal{Intent: intent}
	if !seal.Valid() || seal.DirectoryName() != "uptime-phoenix-probe-"+intent.Identity.OperationID || path.Base(seal.DirectoryName()) != seal.DirectoryName() {
		t.Fatal("local component or identity contract changed")
	}
	for _, bad := range []ProbeBinaryStageSeal{{}, {Intent: ProbeBinaryStageIntent{Identity: intent.Identity, OwnerID: "invalid", AttemptID: intent.AttemptID}}} {
		if bad.Valid() || bad.DirectoryName() != "" {
			t.Fatal("invalid local receipt accepted")
		}
	}
	for _, err := range []error{ErrProbeBinaryStageWriteInput, ErrProbeBinaryStageWriteConflict, ErrProbeBinaryStageWriteUnavailable, ErrProbeBinaryStageWriteUnknown} {
		if err == nil || strings.Contains(err.Error(), "/") {
			t.Fatal("nonfixed local staging error")
		}
	}
}
