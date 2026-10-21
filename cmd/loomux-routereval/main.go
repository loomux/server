// loomux-routereval scores a router tier on the routing-evaluation
// fixtures (LOOM-187): accuracy, latency, tokens and cost. Offline by
// default — against an in-process fake of Anthropic's OpenAI-compatible
// endpoint, checking the harness and measuring request sizes for a cost
// estimate, with no network. --live calls the configured endpoint and
// needs a budget. Not shipped in the image.
//
//	loomux-routereval run --model claude-haiku-5-5 [--live --budget-usd 2] [flags]
//	loomux-routereval compare base.json candidate.json
//
// See docs/design/router-escalation-trial.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/Loomux/server/router/llmrouter"
	"github.com/Loomux/server/router/llmrouter/llmroutertest"
	"github.com/Loomux/server/router/llmrouter/routereval"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

const usage = `usage:
  loomux-routereval run --model ID [--provider openai|anthropic] [--base-url URL] [--api-key-env NAME]
                        [--runs N] [--pace D] [--timeout D] [--cases PREFIX,...] [--fixtures FILE]
                        [--price-in USD --price-out USD] [--budget-usd USD] [--max-calls N]
                        [--out report.json] [--md report.md] [--live]
  loomux-routereval compare BASE.json CANDIDATE.json [--md FILE]
`

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "run":
		err = runCmd(ctx, args[1:], getenv, stdout, stderr)
	case "compare":
		err = compareCmd(args[1:], stdout)
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "loomux-routereval:", err)
		return 1
	}
	return 0
}

type runFlags struct {
	provider, baseURL, model, keyEnv string
	runs, maxCalls                   int
	pace, timeout                    time.Duration
	cases, fixtures, out, md         string
	priceIn, priceOut, budget        float64
	live                             bool
	// price is the price in use, from the flags or the built-in table.
	price routereval.Price
}

func runCmd(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	var f runFlags
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.provider, "provider", "openai", "wire protocol: openai (Chat Completions) or anthropic (Messages API)")
	fs.StringVar(&f.baseURL, "base-url", "https://api.anthropic.com/v1", "endpoint base URL (Anthropic's OpenAI-compatible endpoint by default)")
	fs.StringVar(&f.model, "model", "", "model id, e.g. claude-haiku-5-5 (required)")
	fs.StringVar(&f.keyEnv, "api-key-env", "ROUTEREVAL_API_KEY", "name of the environment variable holding the API key (--live only)")
	fs.IntVar(&f.runs, "runs", 3, "runs per case")
	fs.DurationVar(&f.pace, "pace", time.Second, "wait before each live call")
	fs.DurationVar(&f.timeout, "timeout", 45*time.Second, "per-call timeout, corrective retry included (the escalation tier's)")
	fs.StringVar(&f.cases, "cases", "", "comma-separated case-name prefixes to run (default: all)")
	fs.StringVar(&f.fixtures, "fixtures", "", "fixture file (default: the built-in set)")
	fs.Float64Var(&f.priceIn, "price-in", 0, "input price, USD per million tokens; with --price-out, overrides the built-in price for known models")
	fs.Float64Var(&f.priceOut, "price-out", 0, "output price, USD per million tokens (with --price-in)")
	fs.Float64Var(&f.budget, "budget-usd", 0, "soft cap: stop before a call once the spend plus the dearest call so far would pass this; the provider's spend limit is the hard cap (required with --live)")
	fs.IntVar(&f.maxCalls, "max-calls", 0, "stop after this many calls (0: no cap)")
	fs.StringVar(&f.out, "out", "", "write the JSON report here")
	fs.StringVar(&f.md, "md", "", "write the Markdown report here (default: stdout)")
	fs.BoolVar(&f.live, "live", false, "call the endpoint for real (spends money); without it, run offline against a fake")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if f.model == "" {
		return errors.New("--model is required")
	}
	// One side of a price alone would count the other side's tokens as
	// free (and override a built-in price). Asked for by name, so a zero
	// rate on one side can still be given.
	set := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	if set["price-in"] != set["price-out"] {
		return errors.New("--price-in and --price-out go together: with one alone, the other side's tokens would count as free")
	}
	// Before a zero price falls back to the table, which would hide a
	// negative one.
	if err := (routereval.Price{InputPerMTok: f.priceIn, OutputPerMTok: f.priceOut}).Validate(); err != nil {
		return fmt.Errorf("--price-in/--price-out: %w", err)
	}
	if !routereval.ValidUSD(f.budget) {
		return fmt.Errorf("--budget-usd %v: must be a finite number, 0 or more", f.budget)
	}
	provider, err := llmrouter.ParseProvider(f.provider)
	if err != nil {
		return err
	}
	fx, err := loadFixtures(f.fixtures)
	if err != nil {
		return err
	}
	price, priced := routereval.Price{InputPerMTok: f.priceIn, OutputPerMTok: f.priceOut}, f.priceIn > 0 || f.priceOut > 0
	if !priced {
		price, priced = routereval.PriceFor(f.model)
	}
	f.price = price
	opts := routereval.Options{Runs: f.runs, Timeout: f.timeout, BudgetUSD: f.budget, MaxCalls: f.maxCalls,
		Cases: splitList(f.cases), Log: func(format string, a ...any) { fmt.Fprintf(stderr, format+"\n", a...) }}
	if priced {
		opts.Price = price
	}
	tier := llmrouter.Tier{Provider: provider, BaseURL: f.baseURL, Model: f.model}

	// The offline pass: always for the openai protocol (it's the
	// estimate a live run starts from), and the whole run without --live.
	var estimate *routereval.Estimate
	if provider == llmrouter.ProviderOpenAI {
		fake := llmroutertest.NewAnthropicCompat("offline-key", routereval.Oracle(fx))
		defer fake.Close()
		off := opts
		off.Tier, off.HTTPClient, off.Offline, off.BudgetUSD = tier, fake.Client(), true, 0
		// The fake is Anthropic's compatible endpoint, whatever endpoint
		// the live run names: only the request's size is being measured.
		off.Tier.APIKey, off.Tier.BaseURL = "offline-key", "https://api.anthropic.com/v1"
		if f.live {
			off.Runs, off.MaxCalls, off.Log = 1, 0, nil
		}
		rep, err := routereval.Run(ctx, fx, off)
		if err != nil {
			return err
		}
		if priced {
			e := routereval.EstimateLive(rep, price, f.runs)
			estimate = &e
		}
		if !f.live {
			if err := writeReports(rep, f, stdout); err != nil {
				return err
			}
			printEstimate(stdout, f, estimate, priced)
			if rep.Summary.Passed != rep.Summary.Calls {
				return fmt.Errorf("offline run passed %d of %d calls: the harness or fixtures are broken", rep.Summary.Passed, rep.Summary.Calls)
			}
			return nil
		}
	} else if !f.live {
		return errors.New("offline runs fake the openai protocol only; use --live (with a budget) for an anthropic tier")
	}

	// Live.
	if f.budget <= 0 {
		return errors.New("--live needs --budget-usd")
	}
	if !priced {
		return fmt.Errorf("no built-in price for %q: pass --price-in and --price-out so the budget can be enforced", f.model)
	}
	tier.APIKey = getenv(f.keyEnv)
	if tier.APIKey == "" {
		return fmt.Errorf("%s is not set", f.keyEnv)
	}
	if estimate != nil {
		printEstimate(stderr, f, estimate, priced)
		if estimate.ExpectedUSD > f.budget {
			return fmt.Errorf("the expected cost ($%.2f) is over the budget ($%.2f): lower --runs or narrow --cases", estimate.ExpectedUSD, f.budget)
		}
	}
	opts.Tier, opts.Pace = tier, f.pace
	rep, err := routereval.Run(ctx, fx, opts)
	if err != nil {
		return err
	}
	return writeReports(rep, f, stdout)
}

func printEstimate(w io.Writer, f runFlags, e *routereval.Estimate, priced bool) {
	if !priced {
		fmt.Fprintf(w, "\nNo price for %s: pass --price-in/--price-out for a live-cost estimate.\n", f.model)
		return
	}
	if e == nil {
		return
	}
	source := "built-in price table, unverified: check Anthropic's pricing page"
	if f.priceIn > 0 || f.priceOut > 0 {
		source = "--price-in/--price-out"
	}
	fmt.Fprintf(w, "\nLive estimate for %s, %d runs per case: %d calls, ~%d prompt tokens each; expected $%.2f, worst case $%.2f",
		f.model, f.runs, e.Calls, e.PromptTokensPerCall, e.ExpectedUSD, e.WorstCaseUSD)
	fmt.Fprintf(w, ", at $%.2f/$%.2f per MTok (%s)", f.price.InputPerMTok, f.price.OutputPerMTok, source)
	if f.budget > 0 {
		fmt.Fprintf(w, " (budget $%.2f)", f.budget)
	}
	fmt.Fprintln(w, ".")
}

func writeReports(rep *routereval.Report, f runFlags, stdout io.Writer) error {
	if f.out != "" {
		data, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(f.out, append(data, '\n'), 0o644); err != nil {
			return err
		}
	}
	if f.md == "" {
		rep.WriteMarkdown(stdout)
		return nil
	}
	file, err := os.Create(f.md)
	if err != nil {
		return err
	}
	rep.WriteMarkdown(file)
	return file.Close()
}

func compareCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	md := fs.String("md", "", "write the comparison here (default: stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("compare needs two reports: BASE.json CANDIDATE.json")
	}
	var reports [2]*routereval.Report
	for i, path := range fs.Args() {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &reports[i]); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	w := stdout
	if *md != "" {
		file, err := os.Create(*md)
		if err != nil {
			return err
		}
		defer file.Close()
		w = file
	}
	routereval.WriteComparison(w, reports[0], reports[1])
	return nil
}

func loadFixtures(path string) (*routereval.Fixtures, error) {
	if path == "" {
		return routereval.DefaultFixtures()
	}
	return routereval.LoadFixtures(path)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
