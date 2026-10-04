package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/modelrelay/modelrelay/sdk/go/generated"
)

type kvPair struct {
	Key   string
	Value string
}

func printJSON(payload any) {
	data, _ := json.MarshalIndent(payload, "", "  ")
	fmt.Println(string(data))
}

func printKeyValueTable(pairs []kvPair) {
	fprintKeyValueTable(os.Stdout, pairs)
}

func fprintKeyValueTable(out io.Writer, pairs []kvPair) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "FIELD\tVALUE")
	for _, pair := range pairs {
		if pair.Value == "" {
			continue
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\n", pair.Key, pair.Value)
	}
	_ = w.Flush()
}

func formatUUIDPtr(val *uuid.UUID) string {
	if val == nil {
		return ""
	}
	return val.String()
}

func formatTime(val *time.Time) string {
	if val == nil {
		return ""
	}
	return val.Format(time.RFC3339)
}

func stringOrEmpty(val any) string {
	switch v := val.(type) {
	case *string:
		if v == nil {
			return ""
		}
		return strings.TrimSpace(*v)
	case string:
		return strings.TrimSpace(v)
	case *generated.BillingMode:
		if v == nil {
			return ""
		}
		return string(*v)
	case generated.BillingMode:
		return string(v)
	case *generated.PriceInterval:
		if v == nil {
			return ""
		}
		return string(*v)
	case generated.PriceInterval:
		return string(v)
	default:
		return ""
	}
}
