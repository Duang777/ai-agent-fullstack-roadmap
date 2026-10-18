package sandbox

import (
	"errors"
	"net/netip"
	"strings"
)

// NetPolicy 模仿 E2B 的出站规则：AllowOut / DenyOut 可以写 IP、CIDR；AllowOut 还可以写域名。
// 规则：命中 AllowOut 就放行（优先于 DenyOut）；否则命中 DenyOut 就拒绝；都不命中默认放行。
// 默认放行是 E2B 的默认行为，所以安全的写法几乎总是“DenyOut 全部 + AllowOut 少量”。
type NetPolicy struct {
	AllowOut []string
	DenyOut  []string
}

const AllTraffic = "0.0.0.0/0"

func isDomain(s string) bool {
	if _, err := netip.ParsePrefix(s); err == nil {
		return false
	}
	_, err := netip.ParseAddr(s)
	return err != nil
}

// Validate 检查规则本身是否合理。
func (p NetPolicy) Validate() error {
	hasDomain, denyAll := false, false
	for _, d := range p.DenyOut {
		if isDomain(d) {
			return errors.New("DenyOut 不支持域名：域名只能写在 AllowOut 里")
		}
		if d == AllTraffic {
			denyAll = true
		}
	}
	for _, a := range p.AllowOut {
		if isDomain(a) {
			hasDomain = true
		}
	}
	if hasDomain && !denyAll {
		return errors.New("按域名放行时必须同时拒绝其他全部流量，否则白名单没有意义")
	}
	return nil
}

func match(rule, host string, ip netip.Addr) bool {
	if isDomain(rule) {
		return host != "" && strings.EqualFold(rule, host)
	}
	if pf, err := netip.ParsePrefix(rule); err == nil {
		return ip.IsValid() && pf.Contains(ip)
	}
	a, _ := netip.ParseAddr(rule)
	return ip.IsValid() && a == ip
}

// Allowed 判断一次出站连接。host 是域名（可为空），ip 是解析后的地址。
func (p NetPolicy) Allowed(host string, ip netip.Addr) bool {
	usesDomain := false
	for _, a := range p.AllowOut {
		if isDomain(a) {
			usesDomain = true
		}
		if match(a, host, ip) {
			return true
		}
	}
	// 用了域名规则时，默认 DNS 服务器自动放行，否则域名都解析不了
	if usesDomain && ip == netip.MustParseAddr("8.8.8.8") {
		return true
	}
	for _, d := range p.DenyOut {
		if match(d, host, ip) {
			return false
		}
	}
	return true
}
