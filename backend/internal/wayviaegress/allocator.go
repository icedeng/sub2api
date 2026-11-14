package wayviaegress

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/netip"
)

// Candidate 使用有版本的长度前缀编码；最终绑定以数据库记录为准，禁止启动时重算。
// 算法参考 sub2api-no2api 5e97bf0 的 HMAC 分配思路，本实现独立编写。
func Candidate(key []byte, prefix netip.Prefix, pool, owner string, allocation, binding int64, attempt int) (netip.Addr, error) {
	if len(key) < 32 || !prefix.Addr().Is6() || prefix.Bits() != 64 || attempt < 0 || attempt >= 64 {
		return netip.Addr{}, Error("EGRESS_ALLOCATION_INVALID", "需要至少32字节密钥、/64池和有效冲突序号")
	}
	h := hmac.New(sha256.New, key)
	for _, s := range []string{"wayvia-egress-v1", pool, owner, fmt.Sprint(allocation), fmt.Sprint(binding), fmt.Sprint(attempt)} {
		_ = binary.Write(h, binary.BigEndian, uint32(len(s)))
		_, _ = h.Write([]byte(s))
	}
	b := prefix.Masked().Addr().As16()
	copy(b[8:], h.Sum(nil)[:8])
	return netip.AddrFrom16(b), nil
}
