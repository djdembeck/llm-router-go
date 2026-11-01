package main

import (
	"fmt"
	"testing"
)

func TestVllmKVCapacity(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want float64
	}{
		{"hybrid group aware", `vllm:cache_config_info{engine="0",kv_cache_size_tokens="991561",num_gpu_blocks="1600",block_size="1648"} 1`, 991561},
		{"data parallel sum", "vllm:cache_config_info{engine=\"0\",kv_cache_size_tokens=\"1000\"} 1\nvllm:cache_config_info{engine=\"1\",kv_cache_size_tokens=\"2000\"} 1", 3000},
		{"older engine has no group aware capacity", `vllm:cache_config_info{num_gpu_blocks="1600",block_size="1648"} 1`, 0},
		{"partial engine capacity is unknown", "vllm:cache_config_info{engine=\"0\",kv_cache_size_tokens=\"1000\"} 1\nvllm:cache_config_info{engine=\"1\",kv_cache_size_tokens=\"None\"} 1", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &engineScrape{}
			s.scrapeBody([]byte(tc.body))
			v := s.view
			if tc.want == 0 {
				if v.KVCapTok != nil {
					t.Fatalf("capacity = %v, want unknown", *v.KVCapTok)
				}
			} else if v.KVCapTok == nil || *v.KVCapTok != tc.want {
				t.Fatalf("capacity = %v, want %v", v.KVCapTok, tc.want)
			}
			if v.KVUsedTok != nil {
				t.Fatalf("capacity info invented used tokens: %v", *v.KVUsedTok)
			}
			s.scrapeBody([]byte("vllm:num_requests_running 0"))
			if s.view.KVCapTok != nil {
				t.Fatal("vanished capacity retained as current")
			}
		})
	}
	for _, invalid := range []string{"None", "NaN", "+Inf", "-Inf", "-1", "0", "invalid"} {
		t.Run(invalid, func(t *testing.T) {
			s := &engineScrape{}
			s.scrapeBody([]byte(fmt.Sprintf("vllm:cache_config_info{kv_cache_size_tokens=%q} 1", invalid)))
			if s.view.KVCapTok != nil {
				t.Fatalf("invalid capacity %q accepted", invalid)
			}
		})
	}
}

// TestVllmKVCapacity_SumOverflowStaysFinite: every per-engine capacity is
// individually finite and valid, but the SUM across DP engines overflows to
// +Inf. A published +Inf would make json.Marshal fatal and drop the whole
// /metrics/stream, so the view must stay finite (absent). The normal finite
// sum is covered by "data parallel sum" in TestVllmKVCapacity.
func TestVllmKVCapacity_SumOverflowStaysFinite(t *testing.T) {
	s := &engineScrape{}
	s.scrapeBody([]byte(
		`vllm:cache_config_info{engine="0",kv_cache_size_tokens="1e308"} 1` + "\n" +
			`vllm:cache_config_info{engine="1",kv_cache_size_tokens="1e308"} 1`))
	if v := s.view.KVCapTok; v != nil {
		t.Fatalf("overflowed capacity published as %v, want absent", *v)
	}
}
