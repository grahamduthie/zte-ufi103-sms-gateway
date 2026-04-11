package config

import (
	"fmt"
	"os"
	"strings"
)

const wpaConfPath = "/data/misc/wifi/wpa_supplicant.conf"

// WriteWPAConf generates /data/misc/wifi/wpa_supplicant.conf from a network list.
// Called by setup_mode.go on save and by the web settings handler after a WiFi change.
func WriteWPAConf(networks []WiFiNetCfg) error {
	var sb strings.Builder
	sb.WriteString("ctrl_interface=/data/misc/wifi/sockets\nupdate_config=1\nap_scan=1\n")
	for _, n := range networks {
		sb.WriteString("network={\n")
		sb.WriteString(fmt.Sprintf("    ssid=%q\n", n.SSID))
		if strings.ToUpper(n.Security) == "OPEN" || n.Password == "" {
			sb.WriteString("    key_mgmt=NONE\n")
		} else {
			sb.WriteString(fmt.Sprintf("    psk=%q\n", n.Password))
			sb.WriteString("    key_mgmt=WPA-PSK\n")
		}
		sb.WriteString(fmt.Sprintf("    priority=%d\n", n.Priority))
		sb.WriteString("}\n")
	}
	return os.WriteFile(wpaConfPath, []byte(sb.String()), 0644)
}
