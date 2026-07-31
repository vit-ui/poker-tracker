package uiprompt

import (
	"os/exec"
	"testing"
)

func TestHasDisplay(t *testing.T) {
	tests := []struct {
		name    string
		environ []string
		want    bool
	}{
		{"no display vars", []string{"HOME=/root", "PATH=/usr/bin"}, false},
		{"empty DISPLAY", []string{"DISPLAY="}, false},
		{"x11 display", []string{"DISPLAY=:0"}, true},
		{"wayland display", []string{"WAYLAND_DISPLAY=wayland-0"}, true},
		{"empty wayland only", []string{"WAYLAND_DISPLAY="}, false},
		{"both set", []string{"DISPLAY=:1", "WAYLAND_DISPLAY=wayland-0"}, true},
		{"prefix collision is not a match", []string{"DISPLAYPORT=1"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasDisplay(tt.environ); got != tt.want {
				t.Errorf("hasDisplay(%v) = %v, want %v", tt.environ, got, tt.want)
			}
		})
	}
}

func TestDetectTool(t *testing.T) {
	// look simulates exec.LookPath: found is the set of tools present on PATH.
	look := func(found ...string) func(string) (string, error) {
		set := map[string]bool{}
		for _, f := range found {
			set[f] = true
		}
		return func(name string) (string, error) {
			if set[name] {
				return "/usr/bin/" + name, nil
			}
			return "", exec.ErrNotFound
		}
	}
	display := []string{"DISPLAY=:0"}
	headless := []string{"HOME=/root"}

	tests := []struct {
		name    string
		environ []string
		look    func(string) (string, error)
		want    string
	}{
		{"headless ignores installed tools", headless, look("zenity", "kdialog"), ""},
		{"display + zenity", display, look("zenity"), "zenity"},
		{"display + kdialog only", display, look("kdialog"), "kdialog"},
		{"display prefers zenity over kdialog", display, look("zenity", "kdialog"), "zenity"},
		{"display but no tool", display, look(), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectTool(tt.environ, tt.look); got != tt.want {
				t.Errorf("detectTool = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnvValue(t *testing.T) {
	environ := []string{"HOME=/root", "DISPLAY=:0", "EMPTY="}
	cases := map[string]string{"HOME": "/root", "DISPLAY": ":0", "EMPTY": "", "MISSING": ""}
	for key, want := range cases {
		if got := envValue(environ, key); got != want {
			t.Errorf("envValue(%q) = %q, want %q", key, got, want)
		}
	}
}
