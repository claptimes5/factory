package gates

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Fault is a failure the job's sandbox caused, not the code: a service,
// system library or program the checks need that the factory's setup
// doesn't provide. No model fixes it; factory.yaml or the job image does.
type Fault struct {
	Kind string // service, library, program, or "" when the builder said so
	Line string // the output line that shows it
}

// What says, for people, what the sandbox is missing.
func (f *Fault) What() string {
	switch f.Kind {
	case "service":
		return "a service the checks need (such as a database) isn't running"
	case "library":
		return "a system library the checks need isn't installed in the job image"
	case "program":
		return "a program the checks need isn't installed in the job image"
	default:
		return "the builder found the job's sandbox lacks something the checks need"
	}
}

// ConnRefused is how a check that can't reach a service usually fails.
var ConnRefused = regexp.MustCompile(`(?i)connection refused|econnrefused|could not connect to server|could not translate host name|can't connect to (local )?mysql|getaddrinfo|name or service not known|no such host|redis::cannotconnecterror|server selection timed out`)

var (
	// A refused connection is the sandbox's only when it names a service
	// the factory could start: one to some web API is the code's business.
	serviceSign = regexp.MustCompile(`(?i)postgres|pg::|psycopg|mysql|mariadb|redis|mongo|elasticsearch|rabbitmq|amqp|\b(5432|3306|6379|27017|9200|5672)\b`)
	librarySign = regexp.MustCompile(`(?i)cannot open shared object file|could not open library|library not loaded|dlopen\(.*(failed|not found)|importerror: lib\S*\.so`)
	// The program's name has no slash or dot: a missing path or data file
	// is the code's.
	programSign = regexp.MustCompile(`(?i)(^|\s)[\w+-]+: (command )?not found$|no such file or directory - [\w+-]+$|spawn [\w+-]+ enoent|exec: "[\w+-]+": executable file not found|filenotfounderror: \[errno 2\] no such file or directory: '[\w+-]+'$`)
)

// SetupFault finds, in a failed check's output, the sign of a sandbox
// that lacks what the checks need; nil when there is none.
func SetupFault(output string) *Fault {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		kind := ""
		switch {
		case ConnRefused.MatchString(line) && serviceSign.MatchString(line):
			kind = "service"
		case librarySign.MatchString(line):
			kind = "library"
		case programSign.MatchString(line):
			kind = "program"
		default:
			continue
		}
		if n := 300; len(line) > n {
			for !utf8.RuneStart(line[n]) {
				n--
			}
			line = line[:n] + "…"
		}
		return &Fault{Kind: kind, Line: line}
	}
	return nil
}
