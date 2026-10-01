package pgerr

import (
	"errors"
	"fmt"
	"testing"
)

func TestCodeAndWrapping(t *testing.T) {
	e := New(UniqueViolation, "duplicate key %q", "k").WithDetail("Key (a)=(1) already exists.").WithHint("try again")
	wrapped := fmt.Errorf("context: %w", e)
	if Code(wrapped) != UniqueViolation {
		t.Fatalf("code through wrapping: %s", Code(wrapped))
	}
	if As(wrapped).Detail != "Key (a)=(1) already exists." || As(wrapped).Hint != "try again" {
		t.Fatal("fields lost")
	}
	if Code(errors.New("plain")) != InternalError || As(errors.New("plain")).Message != "plain" {
		t.Fatal("plain errors map to XX000")
	}
}
