package commands

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/github/git-lfs/api"
	"github.com/github/git-lfs/config"
	"github.com/github/git-lfs/errutil"
	"github.com/github/git-lfs/git"
	"github.com/github/git-lfs/httputil"
	"github.com/github/git-lfs/lfs"
	"github.com/github/git-lfs/tools"
	"github.com/spf13/cobra"
)

// Populate man pages
//go:generate go run ../docs/man/mangen.go

var (
	// API is a package-local instance of the API client for use within
	// various command implementations.
	API = api.NewClient(nil)

	Debugging    = false
	ErrorBuffer  = &bytes.Buffer{}
	ErrorWriter  = io.MultiWriter(os.Stderr, ErrorBuffer)
	OutputWriter = io.MultiWriter(os.Stdout, ErrorBuffer)
	RootCmd      = &cobra.Command{
		Use: "git-lfs",
		Run: func(cmd *cobra.Command, args []string) {
			versionCommand(cmd, args)
			cmd.Usage()
		},
	}
	ManPages = make(map[string]string, 20)
	cfg      = config.Config
)

// Error prints a formatted message to Stderr.  It also gets printed to the
// panic log if one is created for this command.
func Error(format string, args ...interface{}) {
	line := format
	if len(args) > 0 {
		line = fmt.Sprintf(format, args...)
	}
	fmt.Fprintln(ErrorWriter, line)
}

// Print prints a formatted message to Stdout.  It also gets printed to the
// panic log if one is created for this command.
func Print(format string, args ...interface{}) {
	line := fmt.Sprintf(format, args...)
	fmt.Fprintln(OutputWriter, line)
}

// Exit prints a formatted message and exits.
func Exit(format string, args ...interface{}) {
	Error(format, args...)
	os.Exit(2)
}

func ExitWithError(err error) {
	if Debugging || errutil.IsFatalError(err) {
		Panic(err, err.Error())
	} else {
		if inner := errutil.GetInnerError(err); inner != nil {
			Error(inner.Error())
		}
		Exit(err.Error())
	}
}

// Debug prints a formatted message if debugging is enabled.  The formatted
// message also shows up in the panic log, if created.
func Debug(format string, args ...interface{}) {
	if !Debugging {
		return
	}
	log.Printf(format, args...)
}

// LoggedError prints a formatted message to Stderr and writes a stack trace for
// the error to a log file without exiting.
func LoggedError(err error, format string, args ...interface{}) {
	Error(format, args...)
	file := handlePanic(err)

	if len(file) > 0 {
		fmt.Fprintf(os.Stderr, "\nErrors logged to %s\nUse `git lfs logs last` to view the log.\n", file)
	}
}

// Panic prints a formatted message, and writes a stack trace for the error to
// a log file before exiting.
func Panic(err error, format string, args ...interface{}) {
	LoggedError(err, format, args...)
	os.Exit(2)
}

func Run() {
	RootCmd.Execute()
	httputil.LogHttpStats(cfg)
}

func Cleanup() {
	if err := lfs.ClearTempObjects(); err != nil {
		fmt.Fprintf(os.Stderr, "Error clearing old temp files: %s\n", err)
	}
}

func PipeMediaCommand(name string, args ...string) error {
	return PipeCommand("bin/"+name, args...)
}

func PipeCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	return cmd.Run()
}

func requireStdin(msg string) {
	var out string

	stat, err := os.Stdin.Stat()
	if err != nil {
		out = fmt.Sprintf("Cannot read from STDIN. %s (%s)", msg, err)
	} else if (stat.Mode() & os.ModeCharDevice) != 0 {
		out = fmt.Sprintf("Cannot read from STDIN. %s", msg)
	}

	if len(out) > 0 {
		Error(out)
		os.Exit(1)
	}
}

func requireInRepo() {
	if !lfs.InRepo() {
		Print("Not in a git repository.")
		os.Exit(128)
	}
}

func handlePanic(err error) string {
	if err == nil {
		return ""
	}

	return logPanic(err)
}

func logPanic(loggedError error) string {
	var fmtWriter io.Writer = os.Stderr

	now := time.Now()
	name := now.Format("20060102T150405.999999999")
	full := filepath.Join(config.LocalLogDir, name+".log")

	if err := os.MkdirAll(config.LocalLogDir, 0755); err != nil {
		full = ""
		fmt.Fprintf(fmtWriter, "Unable to log panic to %s: %s\n\n", config.LocalLogDir, err.Error())
	} else if file, err := os.Create(full); err != nil {
		filename := full
		full = ""
		defer func() {
			fmt.Fprintf(fmtWriter, "Unable to log panic to %s\n\n", filename)
			logPanicToWriter(fmtWriter, err)
		}()
	} else {
		fmtWriter = file
		defer file.Close()
	}

	logPanicToWriter(fmtWriter, loggedError)

	return full
}

func logPanicToWriter(w io.Writer, loggedError error) {
	// log the version
	gitV, err := git.Config.Version()
	if err != nil {
		gitV = "Error getting git version: " + err.Error()
	}

	fmt.Fprintln(w, config.VersionDesc)
	fmt.Fprintln(w, gitV)

	// log the command that was run
	fmt.Fprintln(w)
	fmt.Fprintf(w, "$ %s", filepath.Base(os.Args[0]))
	if len(os.Args) > 0 {
		fmt.Fprintf(w, " %s", strings.Join(os.Args[1:], " "))
	}
	fmt.Fprintln(w)

	// log the error message and stack trace
	w.Write(ErrorBuffer.Bytes())
	fmt.Fprintln(w)

	fmt.Fprintln(w, loggedError.Error())

	if err, ok := loggedError.(ErrorWithStack); ok {
		fmt.Fprintln(w, err.InnerError())
		for key, value := range err.Context() {
			fmt.Fprintf(w, "%s=%s\n", key, value)
		}
		w.Write(err.Stack())
	} else {
		w.Write(errutil.Stack())
	}
	fmt.Fprintln(w, "\nENV:")

	// log the environment
	for _, env := range lfs.Environ() {
		fmt.Fprintln(w, env)
	}
}

type ErrorWithStack interface {
	Context() map[string]string
	InnerError() string
	Stack() []byte
}

func determineIncludeExcludePaths(config *config.Configuration, includeArg, excludeArg string) (include, exclude []string) {
	return tools.CleanPathsDefault(includeArg, ",", config.FetchIncludePaths()),
		tools.CleanPathsDefault(excludeArg, ",", config.FetchExcludePaths())
}

func printHelp(commandName string) {
	if txt, ok := ManPages[commandName]; ok {
		fmt.Fprintf(os.Stderr, "%s\n", strings.TrimSpace(txt))
	} else {
		fmt.Fprintf(os.Stderr, "Sorry, no usage text found for %q\n", commandName)
	}
}

// help is used for 'git-lfs help <command>'
func help(cmd *cobra.Command, args []string) {
	if len(args) == 0 {
		printHelp("git-lfs")
	} else {
		printHelp(args[0])
	}

}

// usage is used for 'git-lfs <command> --help' or when invoked manually
func usage(cmd *cobra.Command) error {
	printHelp(cmd.Name())
	return nil
}

// isCommandEnabled returns whether the environment variable GITLFS<CMD>ENABLED
// is "truthy" according to config.GetenvBool (see
// github.com/github/git-lfs/config#Configuration.GetenvBool), returning false
// by default if the enviornment variable is not specified.
//
// This function call should only guard commands that do not yet have stable
// APIs or solid server implementations.
func isCommandEnabled(cfg *config.Configuration, cmd string) bool {
	return cfg.GetenvBool(fmt.Sprintf("GITLFS%sENABLED", strings.ToUpper(cmd)), false)
}

func init() {
	log.SetOutput(ErrorWriter)
	// Set up help/usage funcs based on manpage text
	RootCmd.SetHelpFunc(help)
	RootCmd.SetHelpTemplate("{{.UsageString}}")
	RootCmd.SetUsageFunc(usage)
}
