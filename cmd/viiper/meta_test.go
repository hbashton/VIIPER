package main

import (
	"strings"
	"testing"
)

func TestDescriptionUsesHbashtonSource(t *testing.T) {
	description := Description()
	if !strings.Contains(description, "Source:  https://github.com/hbashton/VIIPER") {
		t.Fatalf("Description() does not contain the hbashton source URL: %q", description)
	}
	if strings.Contains(strings.ToLower(description), "alia5") {
		t.Fatalf("Description() retains the Alia5 source URL: %q", description)
	}
}
