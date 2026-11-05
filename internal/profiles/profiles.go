// Package profiles holds the built-in device screen profiles.
//
// Android profiles are applied at runtime (display size + density override), so
// any Android profile can be applied to any Android device in milliseconds.
// iOS Simulator screens are fixed per device type, so an iOS profile maps to a
// CoreSimulator device type; switching profile switches to a device of that type.
package profiles

import (
	"fmt"
	"sort"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

var builtins = []api.Profile{
	// Android: pixel sizes and densities of the real devices.
	{Name: "pixel-9-pro", Platform: api.Android, Description: "Google Pixel 9 Pro, 6.3in", Width: 1280, Height: 2856, Density: 480},
	{Name: "pixel-9", Platform: api.Android, Description: "Google Pixel 9, 6.3in", Width: 1080, Height: 2424, Density: 420},
	{Name: "galaxy-s24", Platform: api.Android, Description: "Samsung Galaxy S24, 6.2in FHD+", Width: 1080, Height: 2340, Density: 480},
	{Name: "small-phone", Platform: api.Android, Description: "Budget 6.5in HD+ phone (e.g. Galaxy A05)", Width: 720, Height: 1600, Density: 320},
	{Name: "pixel-fold-inner", Platform: api.Android, Description: "Pixel 9 Pro Fold inner display", Width: 2076, Height: 2152, Density: 390},
	{Name: "pixel-tablet", Platform: api.Android, Description: "Google Pixel Tablet (portrait)", Width: 1600, Height: 2560, Density: 320},

	// iOS: one CoreSimulator device type each. Pixel sizes are the device's native resolution.
	{Name: "iphone-17-pro-max", Platform: api.IOS, Description: "iPhone 17 Pro Max", Width: 1320, Height: 2868, Density: 480, DeviceType: "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro-Max"},
	{Name: "iphone-17-pro", Platform: api.IOS, Description: "iPhone 17 Pro", Width: 1206, Height: 2622, Density: 480, DeviceType: "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro"},
	{Name: "iphone-17", Platform: api.IOS, Description: "iPhone 17", Width: 1206, Height: 2622, Density: 480, DeviceType: "com.apple.CoreSimulator.SimDeviceType.iPhone-17"},
	{Name: "iphone-air", Platform: api.IOS, Description: "iPhone Air", Width: 1260, Height: 2736, Density: 480, DeviceType: "com.apple.CoreSimulator.SimDeviceType.iPhone-Air"},
	{Name: "iphone-16e", Platform: api.IOS, Description: "iPhone 16e", Width: 1170, Height: 2532, Density: 480, DeviceType: "com.apple.CoreSimulator.SimDeviceType.iPhone-16e"},
	{Name: "iphone-se-3", Platform: api.IOS, Description: "iPhone SE (3rd generation), 4.7in", Width: 750, Height: 1334, Density: 320, DeviceType: "com.apple.CoreSimulator.SimDeviceType.iPhone-SE-3rd-generation"},
	{Name: "ipad-mini", Platform: api.IOS, Description: "iPad mini (A17 Pro)", Width: 1488, Height: 2266, Density: 320, DeviceType: "com.apple.CoreSimulator.SimDeviceType.iPad-mini-A17-Pro"},
	{Name: "ipad-pro-11", Platform: api.IOS, Description: "iPad Pro 11-inch (M5)", Width: 1668, Height: 2420, Density: 320, DeviceType: "com.apple.CoreSimulator.SimDeviceType.iPad-Pro-11-inch-M5-12GB"},
}

// All returns every built-in profile, optionally filtered by platform ("" = all).
func All(platform api.Platform) []api.Profile {
	var out []api.Profile
	for _, p := range builtins {
		if platform == "" || p.Platform == platform {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Platform < out[j].Platform })
	return out
}

// Get finds a profile by name.
func Get(name string) (api.Profile, error) {
	for _, p := range builtins {
		if p.Name == name {
			return p, nil
		}
	}
	return api.Profile{}, fmt.Errorf("unknown profile %q (run `agemu profiles`)", name)
}

// Custom builds an ad-hoc Android profile from explicit pixel size and density.
func Custom(width, height, density int) api.Profile {
	return api.Profile{
		Name:        fmt.Sprintf("custom-%dx%d@%d", width, height, density),
		Platform:    api.Android,
		Description: "custom size",
		Width:       width, Height: height, Density: density,
	}
}

// Default returns the profile used for a new device of the platform.
func Default(platform api.Platform) api.Profile {
	if platform == api.IOS {
		p, _ := Get("iphone-17-pro")
		return p
	}
	p, _ := Get("pixel-9")
	return p
}
