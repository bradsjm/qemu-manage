package supervisor

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/bradsjm/qemu-manage/internal/backend"
	"github.com/pterm/pterm"
)

// debugf writes debug output when debug logging is enabled.
func debugf(enabled bool, output io.Writer, format string, args ...any) {
	if !enabled || output == nil {
		return
	}

	logger := pterm.DefaultLogger.
		WithWriter(output).
		WithLevel(pterm.LogLevelDebug).
		WithTime(false)
	logger.Debug(fmt.Sprintf(format, args...))
}

// formatQuotedArgv shell-quotes a command line for debug output
func formatQuotedArgv(path string, args []string) string {
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, strconv.Quote(path))
	for _, arg := range args {
		quoted = append(quoted, strconv.Quote(arg))
	}
	return strings.Join(quoted, " ")
}

// formatManagedCommand omits supervisor-injected trailing arguments from debug logs
func formatManagedCommand(command backend.Command, extraArgsCount int) string {
	if extraArgsCount < 0 || extraArgsCount > len(command.Args) {
		extraArgsCount = 0
	}
	managed := command.Args[:len(command.Args)-extraArgsCount]
	return formatQuotedArgv(command.Path, managed)
}
