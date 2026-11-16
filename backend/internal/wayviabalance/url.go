package wayviabalance

import (
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// normalizeRoot 将模型 API 基址归一为站点根路径；保留租户/反向代理前缀。
// 网络目的地/IP 校验由调用层完成，此处只处理 URL 结构，绝不猜测其它主机。
func normalizeRoot(base string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" || len(base) > 2048 || strings.ContainsAny(base, "\\?#") || strings.IndexFunc(base, unicode.IsSpace) >= 0 {
		return "", ErrInvalidConfig
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Host == "" || strings.HasSuffix(u.Host, ":") || u.RawQuery != "" || u.Fragment != "" {
		return "", ErrInvalidConfig
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || strings.Contains(host, "%") {
		return "", ErrInvalidConfig
	}
	if net.ParseIP(host) == nil {
		if len(host) > 253 {
			return "", ErrInvalidConfig
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", ErrInvalidConfig
			}
			for _, ch := range label {
				if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
					return "", ErrInvalidConfig
				}
			}
		}
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", ErrInvalidConfig
		}
		port = strconv.Itoa(n)
	}
	if port == "443" {
		port = ""
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	// 不接受编码路径，避免代理与上游对编码分隔符/双重编码作不同解释。
	if strings.Contains(u.EscapedPath(), "%") || strings.Contains(u.Path, "//") {
		return "", ErrInvalidConfig
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." || strings.IndexFunc(segment, unicode.IsControl) >= 0 {
			return "", ErrInvalidConfig
		}
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.Path = strings.TrimSuffix(u.Path, "/v1")
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}

// endpointURL 的 path 只能来自适配器固定路径，不接受外部自定义绝对 URL。
func endpointURL(root, path string) string {
	return strings.TrimRight(root, "/") + "/" + strings.TrimLeft(path, "/")
}
