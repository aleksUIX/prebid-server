package exchange

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/currency"
	"github.com/prebid/prebid-server/v4/gdpr"
	"github.com/prebid/prebid-server/v4/hooks"
	"github.com/prebid/prebid-server/v4/hooks/hookexecution"
	metricsConfig "github.com/prebid/prebid-server/v4/metrics/config"
	prometheusmetrics "github.com/prebid/prebid-server/v4/metrics/prometheus"
	"github.com/prebid/prebid-server/v4/modules"
	"github.com/prebid/prebid-server/v4/modules/moduledeps"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/stretchr/testify/require"
)

func TestVastlintAuctionDropsRevenueBid(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	hooksCfg := vastlintHooks(true)
	repo, stages, _, err := modules.NewBuilder().Build(hooksCfg.Modules, moduledeps.ModuleDeps{})
	require.NoError(t, err)
	prom := prometheusmetrics.NewMetrics(config.PrometheusMetrics{}, config.DisabledMetrics{}, nil, stages)

	bad := missingImpressionVAST()
	keptAdm := "https://vast.example/tag.xml"
	bidder := &goodSingleBidder{
		httpRequest: &adapters.RequestData{Method: http.MethodPost, Uri: server.URL, Body: []byte(`{}`), Headers: http.Header{}},
		bidResponse: &adapters.BidderResponse{
			Currency: "USD",
			Bids: []*adapters.TypedBid{
				{BidType: openrtb_ext.BidTypeVideo, Bid: &openrtb2.Bid{ID: "bad", ImpID: "imp-1", Price: 5, AdM: bad}},
				{BidType: openrtb_ext.BidTypeVideo, Bid: &openrtb2.Bid{ID: "kept", ImpID: "imp-1", Price: 1, AdM: keptAdm}},
			},
		},
	}

	ex := &exchange{
		me:                &metricsConfig.NilMetricsEngine{},
		currencyConverter: currency.NewRateConverter(&http.Client{}, time.Second, "", 0),
		bidIDGenerator:    &fakeBidIDGenerator{},
	}
	ex.requestSplitter = requestSplitter{me: ex.me}
	ex.adapterMap = map[openrtb_ext.BidderName]AdaptedBidder{
		openrtb_ext.BidderAppnexus: AdaptBidder(bidder, server.Client(), &config.Configuration{}, &metricsConfig.NilMetricsEngine{}, openrtb_ext.BidderAppnexus, &config.DebugInfo{}, ""),
	}

	w, h := int64(640), int64(360)
	auctionRequest := &AuctionRequest{
		BidRequestWrapper: &openrtb_ext.RequestWrapper{BidRequest: &openrtb2.BidRequest{
			ID: "req-1",
			Imp: []openrtb2.Imp{{
				ID:    "imp-1",
				Video: &openrtb2.Video{MIMEs: []string{"video/mp4"}, W: &w, H: &h},
				Ext:   []byte(`{"prebid":{"bidder":{"appnexus":{"placementid":1}}}}`),
			}},
			Site: &openrtb2.Site{Page: "https://example.test"},
			AT:   1,
			TMax: 500,
			Cur:  []string{"USD"},
		}},
		Account:      config.Account{},
		UserSyncs:    &emptyUsersync{},
		StartTime:    time.Now(),
		HookExecutor: hookexecution.NewHookExecutor(hooks.NewExecutionPlanBuilder(hooksCfg, repo), "/openrtb2/auction", &metricsConfig.NilMetricsEngine{}),
		TCF2Config:   gdpr.NewTCF2Config(config.TCF2{}, config.AccountGDPR{}),
	}

	response, err := ex.HoldAuction(context.Background(), auctionRequest, &DebugLog{})
	require.NoError(t, err)
	require.NotNil(t, response)
	require.Len(t, response.SeatBid, 1)
	require.Len(t, response.SeatBid[0].Bid, 1)
	require.Equal(t, keptAdm, response.SeatBid[0].Bid[0].AdM)
	require.NotContains(t, response.SeatBid[0].Bid[0].AdM, "<VAST")

	require.Equal(t, 1.0, auctionCounter(t, prom, "vastlint_bids_total", map[string]string{"caller": "appnexus", "result": "rejected"}))
	require.Equal(t, 1.0, auctionCounter(t, prom, "vastlint_bids_total", map[string]string{"caller": "appnexus", "result": "skipped"}))
	require.Equal(t, 1.0, auctionCounter(t, prom, "vastlint_findings_total", map[string]string{
		"caller":         "appnexus",
		"rule_id":        "VAST-2.0-inline-impression",
		"revenue_impact": "true",
	}))
}

func vastlintHooks(reject bool) config.Hooks {
	return config.Hooks{
		Enabled: true,
		Modules: config.Modules{
			"openadtech": {
				"vastlint": map[string]interface{}{
					"enabled":        true,
					"reject_revenue": reject,
				},
			},
		},
		HostExecutionPlan: config.HookExecutionPlan{
			Endpoints: map[string]struct {
				Stages map[string]struct {
					Groups []config.HookExecutionGroup `mapstructure:"groups" json:"groups"`
				} `mapstructure:"stages" json:"stages"`
			}{
				"/openrtb2/auction": {
					Stages: map[string]struct {
						Groups []config.HookExecutionGroup `mapstructure:"groups" json:"groups"`
					}{
						"raw_bidder_response": {
							Groups: []config.HookExecutionGroup{{
								Timeout: 20,
								HookSequence: []struct {
									ModuleCode   string `mapstructure:"module_code" json:"module_code"`
									HookImplCode string `mapstructure:"hook_impl_code" json:"hook_impl_code"`
								}{{
									ModuleCode:   "openadtech.vastlint",
									HookImplCode: "vastlint-raw-bidder-response",
								}},
							}},
						},
					},
				},
			},
		},
	}
}

func missingImpressionVAST() string {
	return `<VAST version="2.0"><Ad id="1"><InLine><AdSystem>Test</AdSystem><AdTitle>Test</AdTitle><Creatives><Creative><Linear><Duration>00:00:30</Duration><MediaFiles><MediaFile delivery="progressive" type="video/mp4" width="640" height="360">https://cdn.example.com/ad.mp4</MediaFile></MediaFiles></Linear></Creative></Creatives></InLine></Ad></VAST>`
}

func auctionCounter(t *testing.T, metrics *prometheusmetrics.Metrics, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := metrics.Gatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			got := map[string]string{}
			for _, label := range metric.Label {
				got[label.GetName()] = label.GetValue()
			}
			match := len(got) == len(labels)
			for key, value := range labels {
				if got[key] != value {
					match = false
				}
			}
			if match {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}
