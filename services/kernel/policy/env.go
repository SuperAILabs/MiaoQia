package policy

import (
	"fmt"
	"math/big"
	"strings"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/checker"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
)

// 与 contracts/policy/cel-environment.yaml 的 limits 一致（测试校验）。
const (
	MaxRulesPerPolicy              = 200
	MaxExpressionLength            = 2000
	MaxExpressionStaticCost uint64 = 500_000
	MaxEvaluationCost       uint64 = 1_000_000
)

// 变量名，与 cel-environment.yaml 的 variables 一致。
var variableNames = []string{"fact", "event", "profile", "lists", "vocab", "features", "params", "now"}

// 平台函数名与参数个数，与 cel-environment.yaml 的 functions 一致。
var platformFunctions = map[string]int{
	"category.any_match": 2,
	"region.within":      2,
	"text.contains_any":  2,
	"money.cny":          1,
	"time.days_between":  2,
	"sets.overlaps":      2,
}

// 编译期规模上界，与 cel-environment.yaml 的 size_bounds 一致。
var (
	defaultListBound   uint64 = 100
	defaultStringBound uint64 = 2000
	pathBounds                = map[string]uint64{
		"fact.items":                        500,
		"fact.requirements":                 200,
		"fact.candidates":                   100,
		"fact.org_ids":                      200,
		"fact.winner.credit_flags":          200,
		"fact.winner.history_categories":    500,
		"fact.winner.supplied_brand_owners": 500,
		"fact.winner.past_buyer_ids":        2000,
		"lists":                             20000,
		"vocab":                             1000,
		"params":                            500,
	}
)

var mapDyn = cel.MapType(cel.StringType, cel.DynType)

// NewEnv 返回策略表达式的 CEL 环境。
func NewEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("fact", mapDyn),
		cel.Variable("event", mapDyn),
		cel.Variable("profile", mapDyn),
		cel.Variable("lists", mapDyn),
		cel.Variable("vocab", mapDyn),
		cel.Variable("features", mapDyn),
		cel.Variable("params", mapDyn),
		cel.Variable("now", cel.TimestampType),
		cel.CrossTypeNumericComparisons(true),
		cel.Function("category.any_match",
			cel.Overload("category_any_match_list_list", []*cel.Type{cel.DynType, cel.DynType}, cel.BoolType,
				cel.BinaryBinding(categoryAnyMatch))),
		cel.Function("region.within",
			cel.Overload("region_within_string_list", []*cel.Type{cel.DynType, cel.DynType}, cel.BoolType,
				cel.BinaryBinding(regionWithin))),
		cel.Function("text.contains_any",
			cel.Overload("text_contains_any_string_list", []*cel.Type{cel.DynType, cel.DynType}, cel.BoolType,
				cel.BinaryBinding(textContainsAny))),
		cel.Function("money.cny",
			cel.Overload("money_cny_map", []*cel.Type{cel.DynType}, cel.DoubleType,
				cel.UnaryBinding(moneyCNY))),
		cel.Function("time.days_between",
			cel.Overload("time_days_between_dyn_dyn", []*cel.Type{cel.DynType, cel.DynType}, cel.IntType,
				cel.BinaryBinding(daysBetween))),
		cel.Function("sets.overlaps",
			cel.Overload("sets_overlaps_list_list", []*cel.Type{cel.DynType, cel.DynType}, cel.BoolType,
				cel.BinaryBinding(setsOverlaps))),
	)
}

// ---- 平台函数（纯函数；参数类型不符时返回错误值，求值结果按“未知”处理） ----

func listOf(v ref.Val) (traits.Lister, bool) {
	l, ok := v.(traits.Lister)
	return l, ok
}

func each(l traits.Lister, f func(ref.Val) bool) {
	it := l.Iterator()
	for it.HasNext() == types.True {
		if f(it.Next()) {
			return
		}
	}
}

// native 返回列表的 Go 元素，避免逐元素装箱；无法取得时返回 false。
func native(v ref.Val) ([]any, bool) {
	switch x := v.Value().(type) {
	case []any:
		return x, true
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out, true
	}
	return nil, false
}

// elems 返回列表元素的 Go 值。
func elems(l traits.Lister) []any {
	if xs, ok := native(l); ok {
		return xs
	}
	var out []any
	each(l, func(v ref.Val) bool { out = append(out, v.Value()); return false })
	return out
}

func stringSet(l traits.Lister) (map[string]struct{}, ref.Val) {
	xs := elems(l)
	out := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		s, ok := x.(string)
		if !ok {
			return nil, types.NewErr("期望字符串列表，得到元素类型 %T", x)
		}
		out[s] = struct{}{}
	}
	return out, nil
}

func field(m ref.Val, key string) (ref.Val, bool) {
	mp, ok := m.(traits.Mapper)
	if !ok {
		return nil, false
	}
	return mp.Find(types.String(key))
}

func categoryAnyMatch(items, cats ref.Val) ref.Val {
	il, ok1 := listOf(items)
	cl, ok2 := listOf(cats)
	if !ok1 || !ok2 {
		return types.NewErr("category.any_match 需要两个列表")
	}
	want, bad := stringSet(cl)
	if bad != nil {
		return bad
	}
	matched := false
	each(il, func(item ref.Val) bool {
		// 元素可以是品类本身（视图 fact.categories），也可以是带 categories 的标的
		cs, ok := field(item, "categories")
		if !ok {
			if _, isCat := field(item, "code"); !isCat {
				return false
			}
			cs = types.DefaultTypeAdapter.NativeToValue([]ref.Val{item})
		}
		csl, ok := listOf(cs)
		if !ok {
			return false
		}
		each(csl, func(c ref.Val) bool {
			// 租户品类是行业包产品分类代码；未标明体系或其他体系（如政府采购品目）的代码不可比，不参与匹配
			sc, ok := field(c, "scheme")
			if !ok {
				return false
			}
			if s, _ := sc.Value().(string); !strings.HasPrefix(s, "industry:") {
				return false
			}
			code, ok := field(c, "code")
			if !ok {
				return false
			}
			s, _ := code.Value().(string)
			// 代码本身及其各级上级（按“-”截断）任一在集合中即命中
			for {
				if _, ok := want[s]; ok {
					matched = true
					return true
				}
				i := strings.LastIndex(s, "-")
				if i <= 0 {
					return false
				}
				s = s[:i]
			}
		})
		return matched
	})
	return types.Bool(matched)
}

// regionCovers 判断区划集合中的 r 是否包含地点 loc（GB/T 2260 六位代码）。
func regionCovers(r, loc string) bool {
	if len(r) != 6 || len(loc) < 6 {
		return false
	}
	switch {
	case r == "000000":
		return true
	case strings.HasSuffix(r, "0000"):
		return loc[:2] == r[:2]
	case strings.HasSuffix(r, "00"):
		return loc[:4] == r[:4]
	default:
		return loc[:6] == r
	}
}

func regionWithin(loc, regions ref.Val) ref.Val {
	s, ok := loc.Value().(string)
	if !ok {
		return types.NewErr("region.within 的地点必须是区划代码字符串")
	}
	rl, ok := listOf(regions)
	if !ok {
		return types.NewErr("region.within 的区划集合必须是列表")
	}
	set, bad := stringSet(rl)
	if bad != nil {
		return bad
	}
	for r := range set {
		if regionCovers(r, s) {
			return types.True
		}
	}
	return types.False
}

func textContainsAny(text, terms ref.Val) ref.Val {
	s, ok := text.Value().(string)
	if !ok {
		return types.NewErr("text.contains_any 的文本必须是字符串")
	}
	tl, ok := listOf(terms)
	if !ok {
		return types.NewErr("text.contains_any 的词表必须是列表")
	}
	set, bad := stringSet(tl)
	if bad != nil {
		return bad
	}
	for t := range set {
		if t != "" && strings.Contains(s, t) {
			return types.True
		}
	}
	return types.False
}

func moneyCNY(m ref.Val) ref.Val {
	cur := "CNY"
	if c, ok := field(m, "currency"); ok {
		cur, _ = c.Value().(string)
	}
	if cur != "CNY" {
		return types.NewErr("money.cny：币种 %s 需要汇率视图", cur)
	}
	v, ok := field(m, "value")
	if !ok {
		return types.NewErr("money.cny：金额缺少 value")
	}
	switch x := v.Value().(type) {
	case string:
		f, ok := new(big.Float).SetString(x)
		if !ok {
			return types.NewErr("money.cny：value 不是十进制数 %q", x)
		}
		out, _ := f.Float64()
		return types.Double(out)
	case int64:
		return types.Double(float64(x))
	case float64:
		return types.Double(x)
	}
	return types.NewErr("money.cny：value 类型不支持")
}

func asTime(v ref.Val) (time.Time, error) {
	switch x := v.Value().(type) {
	case time.Time:
		return x, nil
	case string:
		return time.Parse(time.RFC3339, x)
	}
	return time.Time{}, fmt.Errorf("期望时间戳或 RFC 3339 字符串")
}

func daysBetween(a, b ref.Val) ref.Val {
	ta, err := asTime(a)
	if err != nil {
		return types.NewErr("time.days_between：%v", err)
	}
	tb, err := asTime(b)
	if err != nil {
		return types.NewErr("time.days_between：%v", err)
	}
	d := tb.Sub(ta)
	days := int64(d / (24 * time.Hour))
	if d < 0 && d%(24*time.Hour) != 0 {
		days-- // 向下取整
	}
	return types.Int(days)
}

func setsOverlaps(a, b ref.Val) ref.Val {
	al, ok1 := listOf(a)
	bl, ok2 := listOf(b)
	if !ok1 || !ok2 {
		return types.NewErr("sets.overlaps 需要两个列表")
	}
	xs, ys := elems(al), elems(bl)
	if len(xs) > len(ys) {
		xs, ys = ys, xs
	}
	set := make(map[any]struct{}, len(xs)) // 较小一侧集合化，遍历较大一侧
	for _, x := range xs {
		set[x] = struct{}{}
	}
	for _, y := range ys {
		if _, ok := set[y]; ok {
			return types.True
		}
	}
	return types.False
}

// ---- 成本 ----

// staticEstimator 为编译期成本估算提供规模上界与平台函数成本。
type staticEstimator struct{}

func boundFor(path []string) uint64 {
	var parts []string
	for _, p := range path {
		if strings.HasPrefix(p, "@") {
			continue
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return defaultListBound
	}
	switch parts[0] {
	case "lists", "vocab", "params":
		return pathBounds[parts[0]]
	}
	if b, ok := pathBounds[strings.Join(parts, ".")]; ok {
		return b
	}
	return defaultListBound
}

func (staticEstimator) EstimateSize(n checker.AstNode) *checker.SizeEstimate {
	if s := n.ComputedSize(); s != nil {
		return s
	}
	if n.Type() == types.StringType {
		return &checker.SizeEstimate{Min: 0, Max: defaultStringBound}
	}
	b := boundFor(n.Path())
	return &checker.SizeEstimate{Min: 0, Max: b}
}

func (e staticEstimator) size(n checker.AstNode) uint64 {
	return e.EstimateSize(n).Max
}

func (e staticEstimator) EstimateCallCost(function, _ string, _ *checker.AstNode, args []checker.AstNode) *checker.CallEstimate {
	c := callCost(function, func(i int) uint64 {
		if function == "text.contains_any" && i == 0 {
			return max(e.size(args[0]), defaultStringBound) // 动态类型的文本按字符串上界估算，保证静态估算是上界
		}
		return e.size(args[i])
	})
	if c == nil {
		return nil
	}
	return &checker.CallEstimate{CostEstimate: checker.CostEstimate{Min: 1, Max: *c}}
}

// runtimeEstimator 按实际参数规模计算平台函数的运行时成本。
type runtimeEstimator struct{}

func valSize(v ref.Val) uint64 {
	switch x := v.(type) {
	case traits.Sizer:
		if n, ok := x.Size().(types.Int); ok && n > 0 {
			return uint64(n)
		}
	}
	return 1
}

func (runtimeEstimator) CallCost(function, _ string, args []ref.Val, _ ref.Val) *uint64 {
	return callCost(function, func(i int) uint64 {
		if i >= len(args) {
			return 1
		}
		return valSize(args[i])
	})
}

// callCost 是平台函数的成本模型：集合运算按两侧规模之和（实现先把一侧集合化）；
// category.any_match 按每个标的约 4 个品类计；
// 文本匹配按文本长度 × 词数 / 10（与 CEL 字符串运算的 0.1 系数一致）。
func callCost(function string, size func(int) uint64) *uint64 {
	var c uint64
	switch function {
	case "category.any_match":
		c = size(0)*4 + size(1)
	case "region.within":
		c = 1 + size(1)
	case "text.contains_any":
		c = 1 + size(0)*size(1)/10
	case "sets.overlaps":
		c = size(0) + size(1)
	case "money.cny", "time.days_between":
		c = 1
	default:
		return nil
	}
	return &c
}
