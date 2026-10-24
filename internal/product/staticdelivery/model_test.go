package staticdelivery

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestJobTransitionRejectsUncertainFailureOrCancellation(t *testing.T) {
	for _, from := range []JobState{JobActivating, JobActivationUnknown, JobActiveUnverified} {
		for _, to := range []JobState{JobFailed, JobCancelled, JobQueued, JobSucceeded} {
			if ValidJobTransition(from, to) {
				t.Fatalf("unsafe transition %s -> %s", from, to)
			}
		}
	}
	if !ValidJobTransition(JobActivating, JobActivationUnknown) || !ValidJobTransition(JobActivationUnknown, JobVerifying) || !ValidJobTransition(JobVerifying, JobSucceeded) {
		t.Fatal("valid recovery path rejected")
	}
	if ValidJobTransition(JobState("typo"), JobQueued) || ValidJobTransition(JobSucceeded, JobUploading) {
		t.Fatal("unknown/terminal state restarted")
	}
}

func TestPrivateCredentialsNeverSerialize(t *testing.T) {
	c := Credential{Ciphertext: []byte("private-ciphertext"), Nonce: []byte("private-nonce")}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-ciphertext", "private-nonce", "ciphertext", "nonce"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("leaked %s in %s", secret, b)
		}
	}
	b, err = json.Marshal(CompletionToken("private-upload-jwt"))
	if err == nil || strings.Contains(string(b), "private-upload-jwt") {
		t.Fatal("upload JWT must not be serializable")
	}
}

func TestDeliveryErrorsMapWithoutLeakingCause(t *testing.T) {
	err := fmt.Errorf("wrapper: %w", &Error{Code: CodePermissionDenied, Message: "not allowed", Cause: errors.New("Bearer private-token")})
	if StatusForError(err) != http.StatusForbidden || CodeOf(err) != CodePermissionDenied {
		t.Fatal("typed wrapped error lost")
	}
	if strings.Contains(err.Error(), "private-token") {
		t.Fatal("sensitive upstream cause leaked")
	}
	if StatusForError(&Error{Code: CodeIdempotencyRequired}) != http.StatusPreconditionRequired {
		t.Fatal("missing key is not 428")
	}
	if StatusForError(errors.New("unexpected")) != http.StatusInternalServerError {
		t.Fatal("unknown error must fail closed")
	}
}
