package main

import "testing"

func TestDatabaseCredentialsFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name, user, password string
		wantErr              bool
	}{
		{name: "missing both", wantErr: true},
		{name: "missing user", password: "ephemeral-test-input", wantErr: true},
		{name: "missing password", user: "ephemeral-test-user", wantErr: true},
		{name: "explicit credentials", user: "ephemeral-test-user", password: "ephemeral-test-input"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DB_USER", tc.user)
			t.Setenv("DB_PASS", tc.password)
			user, password, err := databaseCredentialsFromEnv()
			if (err != nil) != tc.wantErr {
				t.Fatal("unexpected credential validation result")
			}
			if !tc.wantErr && (user != tc.user || password != tc.password) {
				t.Fatal("explicit credentials were not preserved")
			}
			if tc.wantErr && (user != "" || password != "") {
				t.Fatal("invalid credentials must not be returned")
			}
		})
	}
}
