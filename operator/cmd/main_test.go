package main

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateJobOptions(t *testing.T) {
	tests := []struct {
		name              string
		backoffLimit      int
		activeDeadline    int
		ttlAfterFinished  int
		wantErrorContains string
	}{
		{
			name:             "active deadline one is valid",
			backoffLimit:     0,
			activeDeadline:   1,
			ttlAfterFinished: 0,
		},
		{
			name:              "active deadline zero is rejected",
			backoffLimit:      0,
			activeDeadline:    0,
			ttlAfterFinished:  0,
			wantErrorContains: "--active-deadline-seconds: must be between 1",
		},
		{
			name:             "zero backoff and TTL remain valid",
			backoffLimit:     0,
			activeDeadline:   300,
			ttlAfterFinished: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateJobOptions(tt.backoffLimit, tt.activeDeadline, tt.ttlAfterFinished)
			if tt.wantErrorContains == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErrorContains) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErrorContains, err)
			}
		})
	}
}

func TestResolveWatchNamespace(t *testing.T) {
	readFailure := errors.New("read failed")
	tests := []struct {
		name              string
		explicit          string
		envValue          string
		envPresent        bool
		fileValue         string
		fileError         error
		want              string
		wantErrorContains string
		wantWrappedError  error
	}{
		{
			name:     "explicit namespace",
			explicit: " explicit ",
			want:     "explicit",
		},
		{
			name:       "environment namespace",
			envValue:   " env-namespace\n",
			envPresent: true,
			fileError:  errors.New("file must not be read"),
			want:       "env-namespace",
		},
		{
			name:      "service account file namespace",
			fileValue: " file-namespace\n",
			want:      "file-namespace",
		},
		{
			name:              "missing service account file",
			fileError:         errors.New("not found"),
			wantErrorContains: "reading service-account namespace file",
		},
		{
			name:              "whitespace sources",
			envValue:          " \t",
			envPresent:        true,
			fileValue:         "\n ",
			wantErrorContains: "are empty",
		},
		{
			name:              "service account file error",
			fileError:         readFailure,
			wantErrorContains: "reading service-account namespace file",
			wantWrappedError:  readFailure,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			namespace, err := resolveWatchNamespace(
				tt.explicit,
				"/test/namespace",
				func(name string) (string, bool) {
					if name != "POD_NAMESPACE" {
						t.Fatalf("unexpected environment key %q", name)
					}
					return tt.envValue, tt.envPresent
				},
				func(path string) ([]byte, error) {
					if path != "/test/namespace" {
						t.Fatalf("unexpected namespace file %q", path)
					}
					return []byte(tt.fileValue), tt.fileError
				},
			)
			if tt.wantErrorContains == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if namespace != tt.want {
					t.Fatalf("expected namespace %q, got %q", tt.want, namespace)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErrorContains) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErrorContains, err)
			}
			if tt.wantWrappedError != nil && !errors.Is(err, tt.wantWrappedError) {
				t.Fatalf("expected error to wrap %v, got %v", tt.wantWrappedError, err)
			}
		})
	}
}
