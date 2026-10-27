package main

import (
	"testing"

	"session-memory/internal/vault"
)

func TestWeiyunTokenSurvivesWithoutEnvironment(t *testing.T) {
	a := &App{Keys: vault.MemStore{}}
	t.Setenv(envWeiyunToken, "")
	if a.weiyunToken() != "" {
		t.Fatal("no token expected yet")
	}
	if err := a.saveWeiyunToken(); err == nil {
		t.Fatal("saving without any token must fail")
	}
	t.Setenv(envWeiyunToken, "tok-123")
	if err := a.saveWeiyunToken(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envWeiyunToken, "")
	if got := a.weiyunToken(); got != "tok-123" {
		t.Fatalf("a background run must find the saved token, got %q", got)
	}
	t.Setenv(envWeiyunToken, "fresh")
	if a.weiyunToken() != "fresh" {
		t.Fatal("the environment wins over the saved token")
	}
}
