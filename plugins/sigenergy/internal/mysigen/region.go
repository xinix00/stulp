// Package mysigen implements the small, undocumented part of the mySigen
// owner API that Stulp needs for gateway control.
//
// It deliberately does not expose a generic request method. The API belongs to
// the mySigen app, can change without notice, and includes operations that can
// switch a whole installation. Keeping the allowed hosts and paths here makes
// accidental SSRF and accidental expansion of that control surface harder.
package mysigen

import "fmt"

// Region selects one of the hosts shipped by the official mySigen web client.
type Region string

const (
	RegionEU   Region = "eu"
	RegionAPAC Region = "apac"
	RegionCN   Region = "cn"
	RegionUS   Region = "us"
	RegionAUS  Region = "aus"
	RegionJP   Region = "jp"
)

var regionConfig = map[Region]struct {
	baseURL string
	header  string
}{
	RegionEU:   {"https://api-eu.sigencloud.com/", "eu"},
	RegionAPAC: {"https://api-apac.sigencloud.com/", "apac"},
	RegionCN:   {"https://api-cn.sigenergy.com/", "cn"},
	RegionUS:   {"https://api-us.sigencloud.com/", "us"},
	RegionAUS:  {"https://api-aus.sigencloud.com/", "aus"},
	RegionJP:   {"https://api-jp.sigencloud.com/", "jp"},
}

func regionEndpoint(region Region) (baseURL, header string, err error) {
	config, ok := regionConfig[region]
	if !ok {
		return "", "", fmt.Errorf("onbekende mySigen-regio %q", region)
	}
	return config.baseURL, config.header, nil
}
