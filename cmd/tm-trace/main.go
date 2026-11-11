// tm-trace collects request traces and measures how much of an LLM workload
// a KV cache can reuse. See the README.
package main

import (
	"os"

	"github.com/tensormesh-production/kv-cache-trace-analysis/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
