package stack

import (
	"cmp"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/claptimes5/factory/internal/config"
)

// Fix is repo r, as factory.yaml has it, with what s (read from the
// repo's files) says its jobs' sandbox needs and r doesn't give: the job
// image built from the repo's Dockerfile, the services its compose file
// runs and the settings that find them. It only adds: what r says stays,
// and a repo with services of its own keeps them as they are, since a
// second database beside them would fight them for the settings.
// changes says what, for people; none when there's nothing to add.
func Fix(cfg *config.Config, r config.Repo, s *Suggestion) (out config.Repo, changes []string) {
	out = r
	if s == nil {
		return out, nil
	}
	switch {
	case s.ImageBuild != nil && r.ImageBuild == nil && r.Image == "":
		b := *s.ImageBuild
		b.Secrets = maps.Clone(b.Secrets)
		for _, id := range s.Secrets {
			if f := secretFile(cfg, r, id); f != "" {
				if b.Secrets == nil {
					b.Secrets = map[string]string{}
				}
				b.Secrets[id] = f
			}
		}
		out.ImageBuild, out.ImageFrom = &b, ""
		c := "Build the job image from the repo's " + cmp.Or(b.Dockerfile, "Dockerfile")
		if b.Target != "" {
			c += ", stage " + b.Target
		}
		if r.ImageFrom != "" {
			c += ", instead of on " + r.ImageFrom
		}
		changes = append(changes, c+", so the checks have what the app has installed")
		for _, k := range slices.Sorted(maps.Keys(b.Args)) {
			changes = append(changes, "Build it with "+k+"="+b.Args[k]+", as the repo's CI does")
		}
		for _, id := range slices.Sorted(maps.Keys(b.Secrets)) {
			changes = append(changes, "Give its build the secret "+id+" from "+b.Secrets[id]+" in your checkout")
		}
	case s.ImageFrom != "" && r.ImageFrom == "" && r.ImageBuild == nil && r.Image == "":
		out.ImageFrom = s.ImageFrom
		changes = append(changes, "Build the job image on "+s.ImageFrom+", for the repo's toolchain")
	}
	if len(r.Services) == 0 && len(s.Services) > 0 {
		out.Services = maps.Clone(s.Services)
		for _, n := range slices.Sorted(maps.Keys(s.Services)) {
			changes = append(changes, "Start "+n+" ("+s.Services[n].Image+") for every build and check")
		}
		env := maps.Clone(r.Env)
		for _, k := range slices.Sorted(maps.Keys(s.Env)) {
			if _, set := r.Env[k]; set {
				continue
			}
			if env == nil {
				env = map[string]string{}
			}
			env[k] = s.Env[k]
			changes = append(changes, "Set "+k+"="+s.Env[k]+", so the app finds it")
		}
		out.Env = env
	}
	return out, changes
}

var masterKey = regexp.MustCompile(`(?i)master.?key`)

// secretFile is where build secret id likely is in files.from's checkout,
// as factory.yaml would name it, or "": only Rails' master key is known.
func secretFile(cfg *config.Config, r config.Repo, id string) string {
	if r.Files == nil || !masterKey.MatchString(id) {
		return ""
	}
	from, err := cfg.FilesFrom(r.Files)
	if err != nil {
		return ""
	}
	if _, err := os.Stat(filepath.Join(from, "config", "master.key")); err != nil {
		return ""
	}
	return "config/master.key"
}
