package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// writeTable prints a header and rows: space-padded columns when stdout is
// a terminal, one tab between columns when it is not, so a pipe into cut
// behaves and a terminal reads.
// A nil header is a key-and-value listing with no header line.
// An empty list prints its header and nothing else, never "no results",
// because the header is what tells a script the request succeeded.
func writeTable(w io.Writer, terminal bool, header []string, rows [][]string) error {
	lines := make([][]string, 0, len(rows)+1)
	if header != nil {
		lines = append(lines, header)
	}
	lines = append(lines, rows...)
	if !terminal {
		for _, line := range lines {
			if _, err := fmt.Fprintln(w, strings.Join(line, "\t")); err != nil {
				return err
			}
		}
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, line := range lines {
		if _, err := fmt.Fprintln(tw, strings.Join(line, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}
