package routereval

// Price is a model's list price in US dollars per million tokens.
type Price struct {
	InputPerMTok  float64 `json:"input_per_mtok"`
	OutputPerMTok float64 `json:"output_per_mtok"`
}

// Cost is what prompt and completion tokens cost at p.
func (p Price) Cost(prompt, completion int64) float64 {
	return (float64(prompt)*p.InputPerMTok + float64(completion)*p.OutputPerMTok) / 1e6
}

// knownPrices are list prices for the models the LOOM-187 trial
// considers, copied from a cached model list dated 2026-10-06, not from
// Anthropic's pricing page: verify them there before a live run
// (--price-in/--price-out override them). That the OpenAI-compatible
// endpoint bills as the native API does is assumed too. Claude Haiku
// 5.5's is for prompts up to 100K tokens, far beyond a routing call's.
// Any other model (the current escalation model's included) needs a
// price passed.
var knownPrices = map[string]Price{
	"claude-haiku-4-5":  {InputPerMTok: 1.00, OutputPerMTok: 5.00},
	"claude-haiku-5-5":  {InputPerMTok: 0.10, OutputPerMTok: 0.50},
	"claude-sonnet-5-5": {InputPerMTok: 2.00, OutputPerMTok: 10.00},
}

// PriceFor looks up model's price; ok is false for a model not listed.
func PriceFor(model string) (p Price, ok bool) {
	p, ok = knownPrices[model]
	return p, ok
}
