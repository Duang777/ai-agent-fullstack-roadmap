package cua

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// Guard 在**每个动作执行前**检查：要去的网址是否允许、要点的东西是否需要用户确认。
type Guard struct {
	AllowHosts []string // 允许的域名（含子域名）
	// Confirm 询问用户。返回 false 表示不同意。nil 表示没有人在场：一律拒绝有后果的动作。
	Confirm func(a Action, target string) bool
}

var ErrNeedsConfirmation = errors.New("需要用户确认")

// CheckURL：只允许 http/https；没有 scheme 当作 https；用 URL 解析器判断，不用字符串前缀；
// 拒绝回环、内网、链路本地地址；域名必须在允许列表里。
func (g *Guard) CheckURL(raw string) (string, error) {
	if !strings.Contains(raw, "://") && !strings.HasPrefix(strings.ToLower(raw), "javascript:") &&
		!strings.HasPrefix(strings.ToLower(raw), "data:") && !strings.HasPrefix(strings.ToLower(raw), "file:") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("无法解析网址：%v", err)
	}
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return "", fmt.Errorf("不允许的协议：%s", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return "", fmt.Errorf("不允许访问内网或本机地址：%s", host)
		}
	}
	for _, h := range g.AllowHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return u.String(), nil
		}
	}
	return "", fmt.Errorf("域名不在允许列表：%s", host)
}

// Before 在动作执行前调用。注意它读的是**此刻**的页面，不是模型截图时的页面。
func (g *Guard) Before(b *Browser, a Action) error {
	if a.Kind == "navigate" {
		_, err := g.CheckURL(a.URL)
		return err
	}
	target := ""
	risky := a.NeedsConfirm
	if (a.Kind == "click" || a.Kind == "double_click" || a.Kind == "type") && a.HasXY {
		if e := b.ElementAt(a.X, a.Y); e != nil {
			target = e.Label
			if e.Consequential {
				risky = true
			}
			if e.Href != "" {
				if _, err := g.CheckURL(e.Href); err != nil {
					return err
				}
			}
		}
	}
	if !risky {
		return nil
	}
	if target == "" {
		target = a.Explanation
	}
	if g.Confirm == nil || !g.Confirm(a, target) {
		return fmt.Errorf("%w：%s", ErrNeedsConfirmation, target)
	}
	return nil
}
