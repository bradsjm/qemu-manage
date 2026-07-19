package cli

import (
	"io"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
)

func writeTable(output io.Writer, headers table.Row, rows []table.Row) error {
	writer := table.NewWriter()
	style := table.StyleLight
	style.Color = table.ColorOptions{}
	style.Options.DoNotColorBordersAndSeparators = true
	writer.SetStyle(style)
	writer.AppendHeader(headers)
	writer.AppendRows(rows)
	rendered := strings.TrimRight(writer.Render(), "\n") + "\n"
	_, err := io.WriteString(output, rendered)
	return err
}
