package bgp

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/httpx"
)

// apiBase 是 RIPEstat Data API 的地址，测试里换成本地的假服务。
var apiBase = "https://stat.ripe.net/data"

// sourceApp 放进每个请求的 sourceapp 参数：RIPEstat 要求调用方表明身份，
// 请求量异常时他们先按这个联系，而不是直接限制来源 IP。
const sourceApp = "mibot-lite"

const (
	// callTimeout 是单个接口的时限。这几个接口平时一两秒内返回；偶尔慢的那个不该拖住其余的。
	callTimeout = 10 * time.Second
	// lookupBudget 是一次查询的总时限。IP 查询最长的一条链是 前缀概况 → 上游 → 上游名称，
	// 三步串行；总时限比命令的 30 秒短，留出改消息的时间。
	lookupBudget = 20 * time.Second
	// maxUpstreams 是列出的上游 AS 个数。大网络的上游有几百个，列多了消息就淹没了。
	maxUpstreams = 5
)

// 各接口的响应体上限。asn-neighbours 最大：AS6939 这种大转接网有上万个邻居，约 600 KB。
const (
	smallBody      = 256 << 10
	geoBody        = 1 << 20
	neighboursBody = 2 << 20
)

// call 调一个数据接口，取出应答的 data 部分。RIPEstat 出错时状态码不是 2xx（参数不对是 400），
// 或者 status 不是 "ok"。只解码用得到的字段：大网络的邻居列表有几十万字节，不整个留在内存里。
func call[T any](ctx context.Context, name string, params url.Values, maxBytes int64) (*T, error) {
	params.Set("sourceapp", sourceApp)
	response, err := httpx.Do(ctx, httpx.Request{URL: apiBase + "/" + name + "/data.json?" + params.Encode(), Timeout: callTimeout, MaxBytes: maxBytes})
	if err != nil {
		return nil, err
	}
	if !response.OK() {
		return nil, &httpx.StatusError{Status: response.Status}
	}
	var envelope struct {
		Status string `json:"status"`
		Data   T      `json:"data"`
	}
	if err := json.Unmarshal(response.Body, &envelope); err != nil {
		return nil, fmt.Errorf("%s: invalid JSON: %w", name, err)
	}
	if envelope.Status != "ok" {
		return nil, fmt.Errorf("%s: status %q", name, envelope.Status)
	}
	return &envelope.Data, nil
}

func resource(value string) url.Values { return url.Values{"resource": {value}} }

// prefixOverview 是 prefix-overview 的应答：查 IP 时 resource 是覆盖它的已宣告前缀；
// 没有宣告时 announced 为假，resource 原样是所查的东西。
type prefixOverview struct {
	Announced bool     `json:"announced"`
	Resource  string   `json:"resource"`
	ASNs      []origin `json:"asns"`
	// Related 是公网上宣告的更大或更细的相关前缀，RIPEstat 最多列 100 个；RelatedCount 是总数。
	Related      []string `json:"related_prefixes"`
	RelatedCount int      `json:"actual_num_related"`
}

type origin struct {
	ASN    uint32 `json:"asn"`
	Holder string `json:"holder"`
}

// asOverview 是 as-overview 的应答。未分配的 AS 号 holder 为空。
type asOverview struct {
	Holder string `json:"holder"`
}

// rirList 是 rir 的应答：资源现在归哪个 RIR 管，跨几个 RIR 的前缀有几条，未分配的为空。
// prefix-overview 和 as-overview 里也有个 block，但那是 IANA 那一级的分段，不跟着 RIR 之间的
// 转移走：AS4134 所在的号段写着 ARIN，AS4134 本身早已归 APNIC。
type rirList struct {
	RIRs []struct {
		RIR string `json:"rir"`
	} `json:"rirs"`
}

// neighbourList 是 asn-neighbours 的应答。type 为 left 的邻居在 AS 路径里位于所查 AS 的左边，
// 也就是离观测点更近的一侧：上游（或对等）；right 是下游；uncertain 是分不清的。
// power 是看到这条邻接关系的观测点数，用来给上游排序。
type neighbourList struct {
	Neighbours []neighbour `json:"neighbours"`
}

type neighbour struct {
	ASN   uint32 `json:"asn"`
	Type  string `json:"type"`
	Power int    `json:"power"`
}

// reverseDNS 是 reverse-dns-ip 的应答。查不到时 result 为 null，error 里是解析器的原话，
// 比如「Domain name doesn't exist (NXDOMAIN): …」。
type reverseDNS struct {
	Result []string `json:"result"`
	Error  string   `json:"error"`
}

// geoLite 是 maxmind-geo-lite 的应答。查一个 IP 只有一个位置；查前缀可能有几十个，
// 各带覆盖比例。任播地址的国家是 "?"，城市现在基本都是空的。
type geoLite struct {
	Located []struct {
		Locations []location `json:"locations"`
	} `json:"located_resources"`
}

type location struct {
	Country string  `json:"country"`
	City    string  `json:"city"`
	Covered float64 `json:"covered_percentage"`
}

// prefixCounts 是 ris-prefixes 在 list_prefixes=false 时的应答：只有数量。announced-prefixes
// 会把每个前缀连同时间线列出来，AS13335 就有 600 多 KB、要好几秒，这里只要个数。
type prefixCounts struct {
	Counts struct {
		V4 struct {
			Originating int `json:"originating"`
		} `json:"v4"`
		V6 struct {
			Originating int `json:"originating"`
		} `json:"v6"`
	} `json:"counts"`
}

// upstreams 是一个 AS 的上游：total 个里按 power 取前几个。
type upstreams struct {
	of    uint32
	total int
	top   []peer
}

type peer struct {
	asn  uint32
	name string
}

// report 是一次查询取到的数据。某一项为 nil 是没有查（不适用）或者查失败了，失败的名字按显示顺序记在 failed 里。
type report struct {
	target   target
	prefix   *prefixOverview // IP、前缀
	geo      *geoLite        // IP、前缀
	ptr      *reverseDNS     // 只有 IP
	rir      *rirList        // 都查
	as       *asOverview     // AS 号
	counts   *prefixCounts   // AS 号
	upstream *upstreams      // 起源 AS（IP、前缀）或所查 AS 的上游
	failed   []string
}

// part 是查询里的一项及其结果，用来汇总哪些失败了。
type part struct {
	name string
	err  error
}

// lookup 并发调各个接口。一项失败不影响其他项，只在全部失败时返回错误。
func lookup(ctx context.Context, logger *slog.Logger, t target) (report, error) {
	r := report{target: t}
	query := t.String()
	var wg sync.WaitGroup
	var parts []part
	if t.kind == kindASN {
		var asErr, rirErr, upErr, countErr error
		wg.Go(func() { r.as, asErr = call[asOverview](ctx, "as-overview", resource(query), smallBody) })
		wg.Go(func() { r.rir, rirErr = call[rirList](ctx, "rir", resource(query), smallBody) })
		wg.Go(func() { r.upstream, upErr = fetchUpstreams(ctx, logger, t.asn) })
		wg.Go(func() {
			params := resource(query)
			// 只数自己宣告的（o），并且和 announced-prefixes 一样滤掉只有极少观测点看得到的。
			params.Set("list_prefixes", "false")
			params.Set("types", "o")
			params.Set("noise", "filter")
			r.counts, countErr = call[prefixCounts](ctx, "ris-prefixes", params, smallBody)
		})
		wg.Wait()
		parts = []part{{"AS 信息", asErr}, {"RIR", rirErr}, {"宣告前缀", countErr}, {"上游", upErr}}
	} else {
		var routeErr, upErr, geoErr, rirErr, ptrErr error
		wg.Go(func() {
			r.prefix, routeErr = call[prefixOverview](ctx, "prefix-overview", resource(query), smallBody)
			// 上游要等知道起源 AS 才能查。一个前缀有多个起源时只查第一个。
			if routeErr == nil && len(r.prefix.ASNs) > 0 {
				r.upstream, upErr = fetchUpstreams(ctx, logger, r.prefix.ASNs[0].ASN)
			}
		})
		wg.Go(func() { r.geo, geoErr = call[geoLite](ctx, "maxmind-geo-lite", resource(query), geoBody) })
		wg.Go(func() { r.rir, rirErr = call[rirList](ctx, "rir", resource(query), smallBody) })
		if t.kind == kindIP {
			wg.Go(func() { r.ptr, ptrErr = fetchPTR(ctx, query) })
		}
		wg.Wait()
		parts = []part{{"路由", routeErr}, {"地区", geoErr}, {"RIR", rirErr}, {"反向解析", ptrErr}, {"上游", upErr}}
	}
	var first error
	for _, p := range parts {
		if p.err == nil {
			continue
		}
		if first == nil {
			first = p.err
		}
		r.failed = append(r.failed, p.name)
		warn(logger, "bgp.part_failed", p.name, p.err)
	}
	if r.prefix == nil && r.geo == nil && r.rir == nil && r.ptr == nil && r.as == nil && r.counts == nil && r.upstream == nil {
		return r, first
	}
	return r, nil
}

// fetchUpstreams 查 asn 的上游：按 power 排序取前几个，再一次查出它们的名字。
func fetchUpstreams(ctx context.Context, logger *slog.Logger, asn uint32) (*upstreams, error) {
	list, err := call[neighbourList](ctx, "asn-neighbours", resource(asLabel(asn)), neighboursBody)
	if err != nil {
		return nil, err
	}
	left := slices.DeleteFunc(list.Neighbours, func(n neighbour) bool { return n.Type != "left" })
	slices.SortFunc(left, func(a, b neighbour) int {
		return cmp.Or(cmp.Compare(b.Power, a.Power), cmp.Compare(a.ASN, b.ASN))
	})
	result := &upstreams{of: asn, total: len(left)}
	for _, n := range left[:min(len(left), maxUpstreams)] {
		result.top = append(result.top, peer{asn: n.ASN})
	}
	if len(result.top) == 0 {
		return result, nil
	}
	// as-names 一次收多个 AS 号，五个上游的名字一个请求就够。
	labels := make([]string, len(result.top))
	for index, p := range result.top {
		labels[index] = asLabel(p.asn)
	}
	names, err := call[struct {
		Names map[string]string `json:"names"`
	}](ctx, "as-names", resource(strings.Join(labels, ",")), smallBody)
	if err != nil {
		// 名字只是方便看，查不到就只列 AS 号，不算这一项失败。
		warn(logger, "bgp.names_failed", "上游名称", err)
		return result, nil
	}
	for index := range result.top {
		result.top[index].name = names.Names[strconv.FormatUint(uint64(result.top[index].asn), 10)]
	}
	return result, nil
}

// fetchPTR 查一个 IP 的反向解析。
func fetchPTR(ctx context.Context, ip string) (*reverseDNS, error) {
	return call[reverseDNS](ctx, "reverse-dns-ip", resource(ip), smallBody)
}

func warn(logger *slog.Logger, event, name string, err error) {
	if logger != nil {
		logger.Warn(event, "part", name, "error", err.Error())
	}
}
