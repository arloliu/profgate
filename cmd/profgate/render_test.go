package main

import (
	"bytes"
	"testing"
)

func TestWriteTable(t *testing.T) {
	tests := []struct {
		name     string
		terminal bool
		header   []string
		rows     [][]string
		want     string
	}{
		{
			name:     "a pipe separates columns with one tab",
			terminal: false,
			header:   []string{"POD", "NODE", "VERSION"},
			rows:     [][]string{{"checkout-7c8f8c9b9-xabcd", "worker-07", "1.42.3"}, {"checkout-2", "worker-03", ""}},
			want:     "POD\tNODE\tVERSION\ncheckout-7c8f8c9b9-xabcd\tworker-07\t1.42.3\ncheckout-2\tworker-03\t\n",
		},
		{
			name:     "a terminal pads columns to the widest cell",
			terminal: true,
			header:   []string{"POD", "NODE", "VERSION"},
			rows:     [][]string{{"checkout-7c8f8c9b9-xabcd", "worker-07", "1.42.3"}, {"checkout-2", "worker-03", "1.42.3"}},
			want:     "POD                       NODE       VERSION\ncheckout-7c8f8c9b9-xabcd  worker-07  1.42.3\ncheckout-2                worker-03  1.42.3\n",
		},
		{
			name:     "an empty list prints its header alone",
			terminal: false,
			header:   []string{"NAMESPACE"},
			rows:     nil,
			want:     "NAMESPACE\n",
		},
		{
			name:     "an empty list on a terminal prints its header alone",
			terminal: true,
			header:   []string{"NAMESPACE"},
			rows:     nil,
			want:     "NAMESPACE\n",
		},
		{
			name:     "no header is key and value rows",
			terminal: true,
			rows:     [][]string{{"principal", "alice"}, {"namespaces", "payments"}},
			want:     "principal   alice\nnamespaces  payments\n",
		},
		{
			name:     "no header under a pipe",
			terminal: false,
			rows:     [][]string{{"principal", "alice"}, {"namespaces", "payments"}},
			want:     "principal\talice\nnamespaces\tpayments\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeTable(&buf, tc.terminal, tc.header, tc.rows); err != nil {
				t.Fatal(err)
			}
			if buf.String() != tc.want {
				t.Fatalf("writeTable = %q, want %q", buf.String(), tc.want)
			}
		})
	}
}
