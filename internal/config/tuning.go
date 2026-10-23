package config

import (
	"bytes"
	"strings"
)

// Tunable says whether the tuning loop may propose a change at path: any
// setting the dashboard may change, except its own, whether the
// orchestrator runs, auto-merge, and the routes the orchestrator and the
// tuning loop run on. Those decide what the factory does without you, or
// how it judges itself, so they stay your call.
func Tunable(path []string) bool {
	if len(path) == 0 {
		return false
	}
	switch {
	case path[0] == "tuning", path[0] == "auto_merge":
		return false
	case len(path) >= 2 && path[0] == "orchestrator" && (path[1] == "enabled" || path[1] == "keep_files_for"):
		// Turning it on spends, and keep_files_for removes what people sent.
		return false
	case len(path) >= 2 && path[0] == "routing" && (path[1] == "tune" || path[1] == "orchestrate"):
		return false
	}
	return true
}

// Proposed applies proposed changes to a factory.yaml's bytes, one at a
// time, keeping each only if the tuning loop may change it (see Tunable
// and Settings), its value fits, and the whole file still loads. It
// returns the edited file and, for each change, why it was dropped, or ""
// when it was kept. dir is the file's directory, for validation.
func Proposed(src []byte, dir string, changes []Change) ([]byte, []string) {
	out := src
	why := make([]string, len(changes))
	for i, c := range changes {
		groups, err := Settings(out)
		if err != nil {
			why[i] = err.Error()
			continue
		}
		s, ok := FindSetting(groups, c.Path)
		if !ok || !Tunable(c.Path) {
			why[i] = strings.Join(c.Path, ".") + " isn't a setting tuning may change"
			continue
		}
		v, err := s.Coerce(c.Value)
		if err != nil {
			why[i] = err.Error()
			continue
		}
		next, err := Edit(out, []Change{{Path: c.Path, Value: v}})
		if err != nil {
			why[i] = err.Error()
			continue
		}
		if bytes.Equal(next, out) {
			why[i] = NoChange
			continue
		}
		if _, err := ParseIn(next, dir); err != nil {
			msg := err.Error()
			if len(msg) > 300 {
				msg = msg[:300] + "…"
			}
			why[i] = "factory.yaml would not load: " + msg
			continue
		}
		out = next
	}
	return out, why
}

// NoChange is why Proposed drops a change the file already makes.
const NoChange = "factory.yaml already says that"
