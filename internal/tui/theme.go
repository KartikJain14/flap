package tui

import (
	"flap/internal/canvas"
)

type rgb = canvas.RGB

var hex = canvas.Hex

// theme is the colour scheme of the scene. The sky changes with the
// difficulty stage, from morning to an aurora night.
type theme struct {
	name string

	skyTop, skyMid, skyBottom rgb
	midAt                     float64

	sun, sunGlow     rgb
	sunX, sunY, sunR float64 // fractions of the sky width/height
	glow             float64 // glow opacity
	moon             float64 // 0 = sun, 1 = moon (craters)

	stars  float64 // star opacity
	aurora float64 // aurora opacity

	cloud, cloudShade         rgb
	hillFar, hillNear, hillNS rgb

	grassLight, grassDark, grassEdge rgb
	dirt, dirtDark                   rgb

	pipeLight float64 // brightness multiplier for pipes
}

var themes = []theme{
	{
		name:   "Morning",
		skyTop: hex(0x4aa3df), skyMid: hex(0x8fd0f0), skyBottom: hex(0xd8f2ff), midAt: 0.55,
		sun: hex(0xfff6cc), sunGlow: hex(0xfff0a8), sunX: 0.82, sunY: 0.22, sunR: 0.075, glow: 0.45,
		cloud: hex(0xffffff), cloudShade: hex(0xd7e8f6),
		hillFar: hex(0x9fd3b5), hillNear: hex(0x5fb070), hillNS: hex(0x4c975b),
		grassLight: hex(0x9be35f), grassDark: hex(0x74c947), grassEdge: hex(0x4f9a35),
		dirt: hex(0xdcb97c), dirtDark: hex(0xc59d5f),
		pipeLight: 1,
	},
	{
		name:   "Midday",
		skyTop: hex(0x2c7fd4), skyMid: hex(0x67bdf2), skyBottom: hex(0xc4ebff), midAt: 0.5,
		sun: hex(0xfffdf0), sunGlow: hex(0xfff7c2), sunX: 0.64, sunY: 0.13, sunR: 0.065, glow: 0.5,
		cloud: hex(0xffffff), cloudShade: hex(0xdcebf8),
		hillFar: hex(0x8ccaa6), hillNear: hex(0x52a462), hillNS: hex(0x418a50),
		grassLight: hex(0xa2e866), grassDark: hex(0x77cc49), grassEdge: hex(0x4f9a35),
		dirt: hex(0xe0bd80), dirtDark: hex(0xc9a062),
		pipeLight: 1.02,
	},
	{
		name:   "Golden Hour",
		skyTop: hex(0x3d5bab), skyMid: hex(0xee9c63), skyBottom: hex(0xffd79c), midAt: 0.62,
		sun: hex(0xffdd85), sunGlow: hex(0xffb35c), sunX: 0.8, sunY: 0.58, sunR: 0.095, glow: 0.55,
		cloud: hex(0xffe4c8), cloudShade: hex(0xf2b58e),
		hillFar: hex(0xb48ea2), hillNear: hex(0x6e8f4f), hillNS: hex(0x5b7a40),
		grassLight: hex(0xb5d65c), grassDark: hex(0x8db544), grassEdge: hex(0x5f8530),
		dirt: hex(0xd9a46c), dirtDark: hex(0xbf8752),
		pipeLight: 0.95,
	},
	{
		name:   "Nightfall",
		skyTop: hex(0x0a1135), skyMid: hex(0x1e2a66), skyBottom: hex(0x3b4b8c), midAt: 0.6,
		sun: hex(0xf3f1e4), sunGlow: hex(0x9fb2ff), sunX: 0.78, sunY: 0.2, sunR: 0.06, glow: 0.3, moon: 1,
		stars: 1,
		cloud: hex(0x3b4778), cloudShade: hex(0x2d3762),
		hillFar: hex(0x26325f), hillNear: hex(0x1c2649), hillNS: hex(0x161e3b),
		grassLight: hex(0x4a8a44), grassDark: hex(0x3a7536), grassEdge: hex(0x2a5a28),
		dirt: hex(0x6e5c46), dirtDark: hex(0x5b4b39),
		pipeLight: 0.72,
	},
	{
		name:   "Aurora",
		skyTop: hex(0x040920), skyMid: hex(0x0c2848), skyBottom: hex(0x1b415e), midAt: 0.55,
		sun: hex(0xeef4ff), sunGlow: hex(0x7fe8ff), sunX: 0.2, sunY: 0.16, sunR: 0.045, glow: 0.25, moon: 1,
		stars: 0.85, aurora: 1,
		cloud: hex(0x22385a), cloudShade: hex(0x1a2c48),
		hillFar: hex(0x15253f), hillNear: hex(0x0f1b31), hillNS: hex(0x0b1526),
		grassLight: hex(0x3f8a6a), grassDark: hex(0x2f7357), grassEdge: hex(0x215a43),
		dirt: hex(0x5a4e44), dirtDark: hex(0x4a3f37),
		pipeLight: 0.68,
	},
}

// themeFor returns the theme of a difficulty stage. Extra configured stages
// reuse the last theme.
func themeFor(stage int) theme {
	return themes[min(max(stage, 0), len(themes)-1)]
}

func lerpF(a, b, t float64) float64 { return a + (b-a)*t }

// blendTheme interpolates between two themes.
func blendTheme(a, b theme, t float64) theme {
	if t <= 0 {
		return a
	}
	if t >= 1 {
		return b
	}
	c := func(x, y rgb) rgb { return x.Lerp(y, t) }
	out := b
	out.skyTop, out.skyMid, out.skyBottom = c(a.skyTop, b.skyTop), c(a.skyMid, b.skyMid), c(a.skyBottom, b.skyBottom)
	out.midAt = lerpF(a.midAt, b.midAt, t)
	out.sun, out.sunGlow = c(a.sun, b.sun), c(a.sunGlow, b.sunGlow)
	out.sunX, out.sunY, out.sunR = lerpF(a.sunX, b.sunX, t), lerpF(a.sunY, b.sunY, t), lerpF(a.sunR, b.sunR, t)
	out.glow, out.moon = lerpF(a.glow, b.glow, t), lerpF(a.moon, b.moon, t)
	out.stars, out.aurora = lerpF(a.stars, b.stars, t), lerpF(a.aurora, b.aurora, t)
	out.cloud, out.cloudShade = c(a.cloud, b.cloud), c(a.cloudShade, b.cloudShade)
	out.hillFar, out.hillNear, out.hillNS = c(a.hillFar, b.hillFar), c(a.hillNear, b.hillNear), c(a.hillNS, b.hillNS)
	out.grassLight, out.grassDark, out.grassEdge = c(a.grassLight, b.grassLight), c(a.grassDark, b.grassDark), c(a.grassEdge, b.grassEdge)
	out.dirt, out.dirtDark = c(a.dirt, b.dirt), c(a.dirtDark, b.dirtDark)
	out.pipeLight = lerpF(a.pipeLight, b.pipeLight, t)
	return out
}

// UI palette, shared by every screen.
var (
	colCard       = hex(0x10162f)
	colCardBorder = hex(0x34417a)
	colCardTitle  = hex(0xffd447)
	colText       = hex(0xeef2ff)
	colMuted      = hex(0x8e9ac8)
	colFaint      = hex(0x5d6896)
	colAccent     = hex(0xffd447)
	colAccent2    = hex(0x6fd3ff)
	colGood       = hex(0x6be3a0)
	colBad        = hex(0xff6b6b)
	colWarn       = hex(0xffb347)
	colKeyBg      = hex(0xffd447)
	colKeyFg      = hex(0x241a00)
	colRowHi      = hex(0x23306a)
	colGold       = hex(0xffd447)
	colSilver     = hex(0xcfd8ea)
	colBronze     = hex(0xe39b5d)

	colPipeEdge  = hex(0x2a5c1c)
	colPipeLight = hex(0xa8e866)
	colPipeBase  = hex(0x5db634)
	colPipeShade = hex(0x3f8b25)

	colScore        = hex(0xffffff)
	colScoreOutline = hex(0x2b1d0e)
)
