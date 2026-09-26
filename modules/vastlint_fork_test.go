package modules

import (
	"context"
	"os"
	"testing"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/hooks/hookstage"
	prometheusmetrics "github.com/prebid/prebid-server/v4/metrics/prometheus"
	"github.com/prebid/prebid-server/v4/modules/moduledeps"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

const missingImpressionVAST = `<VAST version="2.0"><Ad id="1"><InLine><AdSystem>Test</AdSystem><AdTitle>Test</AdTitle><Creatives><Creative><Linear><Duration>00:00:30</Duration><MediaFiles><MediaFile delivery="progressive" type="video/mp4" width="640" height="360">https://cdn.example.com/ad.mp4</MediaFile></MediaFiles></Linear></Creative></Creatives></InLine></Ad></VAST>`

func TestVastlintHookRejectsMissingImpression(t *testing.T) {
	hook, metrics := enableVastlint(t, true)
	payload := videoPayload("dsp", missingImpressionVAST)

	result, err := hook.HandleRawBidderResponseHook(context.Background(), hookstage.ModuleInvocationContext{}, payload)
	require.NoError(t, err)
	require.Empty(t, applyBidMutations(t, payload, result).BidderResponse.Bids)
	require.Equal(t, 1.0, findingCount(t, metrics, "dsp", "VAST-2.0-inline-impression"))
}

func TestVastlintHookCountsAndKeepsBid(t *testing.T) {
	hook, metrics := enableVastlint(t, false)
	payload := videoPayload("dsp", missingImpressionVAST)

	result, err := hook.HandleRawBidderResponseHook(context.Background(), hookstage.ModuleInvocationContext{}, payload)
	require.NoError(t, err)
	require.Empty(t, result.ChangeSet.Mutations())
	require.Len(t, payload.BidderResponse.Bids, 1)
	require.Equal(t, 1.0, findingCount(t, metrics, "dsp", "VAST-2.0-inline-impression"))
}

func TestVastlintHookOnVideoStormFixture(t *testing.T) {
	body, err := os.ReadFile("testdata/videostorm_simid_4.2.xml")
	require.NoError(t, err)

	hook, metrics := enableVastlint(t, false)
	payload := videoPayload("videostorm", string(body))

	result, err := hook.HandleRawBidderResponseHook(context.Background(), hookstage.ModuleInvocationContext{}, payload)
	require.NoError(t, err)
	require.Empty(t, result.Errors)
	require.Empty(t, result.ChangeSet.Mutations())
	require.Equal(t, 1.0, metricValue(t, metrics, "vastlint_bids_total", map[string]string{
		"caller": "videostorm",
		"result": "checked",
	}))

	got := map[string]string{}
	for _, label := range findingLabels(t, metrics) {
		t.Logf("finding caller=%s rule=%s revenue_impact=%s", label.caller, label.ruleID, label.revenue)
		require.Equal(t, "videostorm", label.caller)
		got[label.ruleID] = label.revenue
	}
	require.Equal(t, map[string]string{
		"VAST-2.0-url-cdata":                "false",
		"SIMID-1.0-simid-interactive-start": "false",
		"VAST-2.0-adsystem-no-version":      "false",
	}, got)
}

func enableVastlint(t *testing.T, rejectRevenue bool) (hookstage.RawBidderResponse, *prometheusmetrics.Metrics) {
	t.Helper()
	repo, stages, _, err := NewBuilder().Build(config.Modules{
		"openadtech": {
			"vastlint": map[string]interface{}{
				"enabled":        true,
				"reject_revenue": rejectRevenue,
			},
		},
	}, moduledeps.ModuleDeps{})
	require.NoError(t, err)
	require.Equal(t, []string{"raw_bidder_response"}, stages["openadtech_vastlint"])

	hook, ok := repo.GetRawBidderResponseHook("openadtech.vastlint")
	require.True(t, ok)

	metrics := prometheusmetrics.NewMetrics(config.PrometheusMetrics{}, config.DisabledMetrics{}, nil, stages)
	return hook, metrics
}

func videoPayload(bidder, adm string) hookstage.RawBidderResponsePayload {
	return hookstage.RawBidderResponsePayload{
		Bidder: bidder,
		BidderResponse: &adapters.BidderResponse{Bids: []*adapters.TypedBid{{
			BidType: openrtb_ext.BidTypeVideo,
			Bid:     &openrtb2.Bid{ID: "bid-1", ImpID: "imp-1", AdM: adm},
		}}},
	}
}

func applyBidMutations(t *testing.T, payload hookstage.RawBidderResponsePayload, result hookstage.HookResult[hookstage.RawBidderResponsePayload]) hookstage.RawBidderResponsePayload {
	t.Helper()
	for _, mutation := range result.ChangeSet.Mutations() {
		next, err := mutation.Apply(payload)
		require.NoError(t, err)
		payload = next
	}
	return payload
}

type findingLabel struct {
	caller, ruleID, revenue string
}

func findingLabels(t *testing.T, metrics *prometheusmetrics.Metrics) []findingLabel {
	t.Helper()
	families, err := metrics.Gatherer.Gather()
	require.NoError(t, err)
	var got []findingLabel
	for _, family := range families {
		if family.GetName() != "vastlint_findings_total" {
			continue
		}
		for _, metric := range family.Metric {
			if metric.GetCounter().GetValue() == 0 {
				continue
			}
			labels := labelMap(metric)
			got = append(got, findingLabel{
				caller:  labels["caller"],
				ruleID:  labels["rule_id"],
				revenue: labels["revenue_impact"],
			})
		}
	}
	return got
}

func findingCount(t *testing.T, metrics *prometheusmetrics.Metrics, caller, ruleID string) float64 {
	t.Helper()
	return metricValue(t, metrics, "vastlint_findings_total", map[string]string{
		"caller":         caller,
		"rule_id":        ruleID,
		"revenue_impact": "true",
	})
}

func metricValue(t *testing.T, metrics *prometheusmetrics.Metrics, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := metrics.Gatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			got := labelMap(metric)
			match := true
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

func labelMap(metric *dto.Metric) map[string]string {
	out := make(map[string]string, len(metric.Label))
	for _, label := range metric.Label {
		out[label.GetName()] = label.GetValue()
	}
	return out
}
