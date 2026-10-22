package stack

import (
	"cmp"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/claptimes5/factory/internal/config"
	"gopkg.in/yaml.v3"
)

// testStages are the Dockerfile stages a job image is built on, best
// first: ones made for running the tests, with their tools and system
// libraries. A production stage can set RAILS_ENV or leave out the test
// gems, so it is never picked on its own.
var testStages = []string{"test", "ci", "development", "dev"}

var fromRE = regexp.MustCompile(`(?im)^\s*FROM\s+(?:--\S+\s+)*(\S+)(?:\s+AS\s+(\S+))?\s*$`)

// dockerfile builds the job image from the repo's Dockerfile when it has
// a stage for the tests: what the app needs installed is already there.
// Otherwise it says why it doesn't, for the notes when nothing else does.
func dockerfile(s *Suggestion, read func(string) string, ci string) string {
	df := read("Dockerfile")
	if df == "" {
		return ""
	}
	from := map[string]string{} // stage -> what it starts from
	var stages []string
	for _, m := range fromRE.FindAllStringSubmatch(df, -1) {
		if m[2] != "" {
			from[strings.ToLower(m[2])] = strings.ToLower(m[1])
			stages = append(stages, m[2])
		}
	}
	i := slices.IndexFunc(testStages, func(t string) bool {
		return slices.ContainsFunc(stages, func(st string) bool { return strings.EqualFold(st, t) })
	})
	if i < 0 {
		return "It has a Dockerfile, but no test, ci or development stage, so the job image isn't built from it. To build it from the Dockerfile anyway, set image_build in factory.yaml."
	}
	target := stages[slices.IndexFunc(stages, func(st string) bool { return strings.EqualFold(st, testStages[i]) })]
	// The image the stage starts from, through the stages it builds on.
	base := strings.ToLower(target)
	for n := 0; n < len(stages) && from[base] != ""; n++ {
		base = from[base]
	}
	if strings.Contains(base, "alpine") || strings.Contains(base, "distroless") {
		return "Its Dockerfile's " + target + " stage is built on " + base + ", and job images need Debian or Ubuntu, so the job image isn't built from it."
	}
	s.ImageBuild, s.ImageFrom = &config.ImageBuild{Target: target}, ""
	s.Notes = append(s.Notes, "The job image is built from its Dockerfile's "+target+" stage, so the checks have what the app has installed.")
	// The CI that builds the same stage says what it's built with.
	ciFrom := map[string]string{} // secret id -> where CI takes it from
	targetRE := regexp.MustCompile(`--target[= ]` + regexp.QuoteMeta(target) + `(\s|$)`)
	for _, line := range strings.Split(ci, "\n") {
		if !strings.Contains(line, "docker") || !strings.Contains(line, " build") || !targetRE.MatchString(line) {
			continue
		}
		for _, m := range buildArgRE.FindAllStringSubmatch(line, -1) {
			if !strings.Contains(m[2], "$") {
				if s.ImageBuild.Args == nil {
					s.ImageBuild.Args = map[string]string{}
				}
				s.ImageBuild.Args[m[1]] = strings.Trim(m[2], `"'`)
			}
		}
		for _, m := range ciSecretRE.FindAllStringSubmatch(line, -1) {
			if _, src, ok := strings.Cut(m[2], "="); ok {
				ciFrom[m[1]] = src
			}
		}
	}
	for _, id := range SecretIDs(df) {
		note := "Its Dockerfile uses the build secret " + id
		if src := ciFrom[id]; src != "" {
			note += ", which its CI takes from " + src
		}
		s.Notes = append(s.Notes, note+". Set image_build.secrets."+id+" in factory.yaml to the file that holds it, a path in files.from's checkout.")
	}
	return ""
}

var (
	buildArgRE = regexp.MustCompile(`--build-arg[= ]([A-Za-z_][A-Za-z0-9_]*)=(\S+)`)
	// --secret id=x,env=Y or id=x,src=path: the id, then the rest.
	ciSecretRE = regexp.MustCompile(`--secret[= ]id=([\w.-]+),(\S+)`)
	mountRE    = regexp.MustCompile(`--mount=(\S*type=secret\S*)`)
)

// SecretIDs are the build secrets a Dockerfile mounts, in order.
func SecretIDs(df string) []string {
	var ids []string
	for _, m := range mountRE.FindAllStringSubmatch(df, -1) {
		for _, kv := range strings.Split(m[1], ",") {
			if id, ok := strings.CutPrefix(kv, "id="); ok && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// devcontainerBuild is a devcontainer.json's build, its paths relative to
// dir, the folder the file is in.
func devcontainerBuild(dir, dockerFile, context, target string) *config.ImageBuild {
	if context == "" {
		context = "."
	}
	b := &config.ImageBuild{Dockerfile: path.Join(dir, dockerFile), Context: path.Join(dir, context), Target: target}
	if b.Context == "." {
		b.Context = ""
	}
	return b
}

// composeFiles are the names docker compose looks for.
var composeFiles = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// composeKind is a service setup can start, by image name, with how to
// tell it's ready from the env it's given.
type composeKind struct {
	images []string
	ready  func(env map[string]string) string
}

var composeKinds = []composeKind{
	{[]string{"postgres", "postgis"}, func(env map[string]string) string {
		return "pg_isready -U " + cmp.Or(env["POSTGRES_USER"], "postgres")
	}},
	{[]string{"mysql", "mariadb"}, func(env map[string]string) string {
		if pw := cmp.Or(env["MYSQL_ROOT_PASSWORD"], env["MARIADB_ROOT_PASSWORD"]); pw != "" {
			return "mysqladmin ping -h 127.0.0.1 -p" + pw
		}
		return "mysqladmin ping -h 127.0.0.1"
	}},
	{[]string{"redis", "valkey"}, func(map[string]string) string { return "redis-cli ping" }},
	{[]string{"mongo"}, func(map[string]string) string { return "mongosh --quiet --eval 'db.runCommand({ping:1})'" }},
}

// dbEnv is the app's settings that find a service.
var dbEnv = regexp.MustCompile(`(?i)^(PG|POSTGRES|MYSQL|MARIADB|DATABASE|DB_|REDIS|MONGO)`)

// compose takes the services the tests need, and the app's settings that
// find them, from the repo's compose file: its own names and credentials,
// which config/database.yml and the like already expect. When it has
// any, they replace the ones guessed from the stack.
func compose(s *Suggestion, read func(string) string) {
	var raw, file string
	for _, f := range composeFiles {
		if raw = read(f); raw != "" {
			file = f
			break
		}
	}
	if raw == "" {
		return
	}
	var c struct {
		Services map[string]struct {
			Image       string `yaml:"image"`
			Build       any    `yaml:"build"`
			Environment any    `yaml:"environment"`
		} `yaml:"services"`
	}
	if yaml.Unmarshal([]byte(raw), &c) != nil {
		s.Notes = append(s.Notes, "Its "+file+" couldn't be read, so the services are guessed from the stack.")
		return
	}
	svcs, env := map[string]config.Service{}, map[string]string{}
	var skipped []string
	for _, name := range slices.Sorted(maps.Keys(c.Services)) {
		cs := c.Services[name]
		e := composeEnv(cs.Environment)
		if cs.Build != nil { // the app: its settings, not a service
			for k, v := range e {
				if dbEnv.MatchString(k) {
					env[k] = v
				}
			}
			continue
		}
		if cs.Image == "" {
			continue
		}
		kind := imageName(cs.Image)
		k := slices.IndexFunc(composeKinds, func(ck composeKind) bool { return slices.Contains(ck.images, kind) })
		if k < 0 || !config.ServiceName.MatchString(name) {
			skipped = append(skipped, name)
			continue
		}
		svcs[name] = config.Service{Image: cs.Image, Env: e, Ready: composeKinds[k].ready(e)}
	}
	if len(svcs) == 0 {
		return
	}
	s.Services, s.Env = svcs, env
	if len(env) == 0 {
		s.Env = nil
	}
	note := "Its services and the settings that find them come from " + file + "."
	if len(skipped) > 0 {
		note += " It also runs " + strings.Join(skipped, ", ") + ", which setup doesn't start; add any the tests need under services in factory.yaml."
	}
	s.Notes = append(s.Notes, note)
}

// composeEnv is a compose service's environment, as a map or a list of
// KEY=value. A value that interpolates the shell's variables is left out:
// setup can't know it.
func composeEnv(v any) map[string]string {
	out := map[string]string{}
	add := func(k string, val any) {
		str := ""
		if val != nil {
			str = fmt.Sprint(val) // a number or a bool, too
		}
		if k != "" && !strings.Contains(str, "$") {
			out[k] = str
		}
	}
	switch e := v.(type) {
	case map[string]any:
		for k, val := range e {
			add(k, val)
		}
	case []any:
		for _, kv := range e {
			if str, ok := kv.(string); ok {
				k, val, _ := strings.Cut(str, "=")
				add(k, val)
			}
		}
	}
	return out
}

// imageName is an image reference's name without registry, path or tag:
// docker.io/library/postgres:16 is postgres.
func imageName(ref string) string {
	ref = ref[strings.LastIndex(ref, "/")+1:]
	ref, _, _ = strings.Cut(ref, "@")
	ref, _, _ = strings.Cut(ref, ":")
	return strings.ToLower(ref)
}
