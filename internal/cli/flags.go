package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
)

// exitError ends the program with code once its message has been printed
// (usage errors exit with status 2 like argparse, --help with 0).
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

// flagSet is a pflag.FlagSet with a hand-written usage text and argparse
// conveniences: unique prefixes of long options are accepted (--force for
// --force-recreate).
type flagSet struct {
	*pflag.FlagSet
	usage        string
	interspersed bool
	positionals  []string // set aside by expandAbbreviations, see positionalMark
}

// positionalMark stands, during pflag's parsing, for an argument that argparse
// reads as positional although it starts with "-" (pflag would not).
const positionalMark = "\x00positional:"

func newFlagSet(name, usage string) *flagSet {
	fs := pflag.NewFlagSet(name, pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return &flagSet{FlagSet: fs, usage: usage, interspersed: true}
}

// stopAtFirstArg treats everything from the first positional argument on as
// positional (for `exec SERVICE COMMAND...` and the global options).
func (fs *flagSet) stopAtFirstArg() {
	fs.SetInterspersed(false)
	fs.interspersed = false
}

func (fs *flagSet) fail(err error) error {
	if errors.Is(err, pflag.ErrHelp) {
		fmt.Print(fs.usage)
		return &exitError{0}
	}
	fmt.Fprint(os.Stderr, fs.usage)
	errorMsg(err.Error())
	return &exitError{2}
}

// parse parses args and returns the positional arguments.
func (fs *flagSet) parse(args []string) ([]string, error) {
	args, err := fs.expandAbbreviations(args)
	if err == nil {
		err = fs.Parse(args)
	}
	if err != nil {
		return nil, fs.fail(err)
	}
	positionals := fs.Args()
	for i, arg := range positionals {
		if rest, ok := strings.CutPrefix(arg, positionalMark); ok {
			n, _ := strconv.Atoi(rest)
			positionals[i] = fs.positionals[n]
		}
	}
	return positionals, nil
}

// parseNoArgs parses a command that takes no positional arguments.
func (fs *flagSet) parseNoArgs(args []string) error {
	extra, err := fs.parse(args)
	if err == nil && len(extra) > 0 {
		err = fs.fail(errors.New("unrecognized arguments: " + strings.Join(extra, " ")))
	}
	return err
}

// decimalInt is an int flag parsed in base 10 like argparse's int (pflag's
// own int flags read 010 as octal).
type decimalInt int

func (d *decimalInt) String() string { return strconv.Itoa(int(*d)) }
func (d *decimalInt) Type() string   { return "int" }

func (d *decimalInt) Set(s string) error {
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("invalid int value: %q", s)
	}
	*d = decimalInt(n)
	return nil
}

func (fs *flagSet) decimalIntP(name, shorthand string, value int) *int {
	p := new(int)
	*p = value
	fs.VarP((*decimalInt)(p), name, shorthand, "")
	return p
}

func takesValue(f *pflag.Flag) bool { return f.NoOptDefVal == "" }

var negativeNumberRE = regexp.MustCompile(`^-\d+$|^-\d*\.\d+$`)

// optionValue returns the argument following an option that takes a value. Like
// argparse, it refuses one that looks like an option (pflag would take it).
func optionValue(option string, args []string, i int) (string, error) {
	next := args[i+1]
	if strings.HasPrefix(next, "-") && next != "-" && !negativeNumberRE.MatchString(next) && !strings.Contains(next, " ") {
		return "", fmt.Errorf("argument %s: expected one argument", option)
	}
	return next, nil
}

// expandAbbreviations rewrites unique prefixes of long options to their full
// name, skipping option values and positional arguments.
func (fs *flagSet) expandAbbreviations(args []string) ([]string, error) {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return append(out, args[i:]...), nil

		case strings.HasPrefix(arg, "-") && strings.Contains(arg, " ") && !strings.Contains(arg, "="):
			// argparse reads an argument with a space as positional
			out = append(out, positionalMark+strconv.Itoa(len(fs.positionals)))
			fs.positionals = append(fs.positionals, arg)
			if !fs.interspersed {
				return append(out, args[i+1:]...), nil
			}

		case strings.HasPrefix(arg, "--"):
			name, value, hasValue := strings.Cut(arg[2:], "=")
			if fs.Lookup(name) == nil {
				full, err := fs.matchPrefix(name)
				if err != nil {
					return nil, err
				}
				if full != "" {
					name, arg = full, "--"+full
					if hasValue {
						arg += "=" + value
					}
				}
			}
			out = append(out, arg)
			if f := fs.Lookup(name); f != nil && takesValue(f) && !hasValue && i+1 < len(args) {
				v, err := optionValue("--"+name, args, i)
				if err != nil {
					return nil, err
				}
				i++
				out = append(out, v)
			}

		case strings.HasPrefix(arg, "-") && len(arg) > 1:
			out = append(out, arg)
			// in a group like -ft or -n5, a value-taking flag ends the group;
			// at the very end it consumes the next argument
			for j := 1; j < len(arg); j++ {
				if f := fs.ShorthandLookup(arg[j : j+1]); f != nil && takesValue(f) {
					if j == len(arg)-1 && i+1 < len(args) {
						v, err := optionValue("-"+arg[j:j+1], args, i)
						if err != nil {
							return nil, err
						}
						i++
						out = append(out, v)
					}
					break
				}
			}

		default:
			if !fs.interspersed {
				return append(out, args[i:]...), nil
			}
			out = append(out, arg)
		}
	}
	return out, nil
}

func (fs *flagSet) matchPrefix(prefix string) (string, error) {
	if prefix == "" {
		return "", nil
	}
	var matches []string
	if fs.Lookup("help") == nil && strings.HasPrefix("help", prefix) {
		matches = append(matches, "help") // handled by pflag itself
	}
	fs.VisitAll(func(f *pflag.Flag) {
		if strings.HasPrefix(f.Name, prefix) {
			matches = append(matches, f.Name)
		}
	})
	switch len(matches) {
	case 0:
		return "", nil
	case 1:
		return matches[0], nil
	}
	return "", fmt.Errorf("ambiguous option: --%s could match --%s", prefix, strings.Join(matches, ", --"))
}
