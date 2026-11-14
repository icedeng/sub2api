package wayviaegress

import (
	"encoding/json"
	"fmt"
)

// inspectAddressJSON 分开验证普通主地址和deprecated专用/128，不修改原主地址。
func inspectAddressJSON(raw []byte, address string, primary bool) (bool, bool, error) {
	var entries []struct {
		Info []struct {
			Local     string          `json:"local"`
			Prefix    int             `json:"prefixlen"`
			Flags     []string        `json:"flags"`
			Tentative bool            `json:"tentative"`
			DADFailed bool            `json:"dadfailed"`
			Preferred json.RawMessage `json:"preferred_life_time"`
			Valid     json.RawMessage `json:"valid_life_time"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return false, false, err
	}
	for _, e := range entries {
		for _, i := range e.Info {
			if i.Local != address {
				continue
			}
			if i.DADFailed {
				return true, false, fmt.Errorf("DAD failed")
			}
			ready := !i.Tentative && len(i.Valid) > 0 && string(i.Valid) != "0"
			if primary {
				ready = ready && len(i.Preferred) > 0 && string(i.Preferred) != "0"
			} else {
				ready = ready && i.Prefix == 128 && string(i.Preferred) == "0"
			}
			for _, flag := range i.Flags {
				if flag == "dadfailed" {
					return true, false, fmt.Errorf("DAD failed")
				}
				if flag == "tentative" {
					ready = false
				}
			}
			return true, ready, nil
		}
	}
	return false, false, nil
}
