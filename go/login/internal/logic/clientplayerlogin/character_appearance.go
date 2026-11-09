package clientplayerloginlogic

// validCharacterAppearance 的键是稳定资源身份，不是职业或性别编号。
// 空值保留旧客户端行为；删除人物不在白名单中，不做前缀猜测或重新编号。
func validCharacterAppearance(id string) bool {
	switch id {
	case "", "00_reference_topright_boy", "01_ice_sword_girl", "02_fire_talisman_boy",
		"03_lotus_healer_girl", "04_mountain_guardian_boy", "05_celestial_musician_girl",
		"06_thunder_caster_boy", "08_alchemy_prodigy_boy", "09_bamboo_archer_girl",
		"10_crimson_spear_girl", "15_water_dragon_scholar_boy", "17_ghost_script_calligrapher_boy":
		return true
	default:
		return false
	}
}

// 退役身份只用于确认已提交建角的丢响应重试。
func retiredCharacterAppearance(id string) bool {
	switch id {
	case "07_moon_shadow_assassin_girl", "14_short_hair_snow_summoner_girl", "20_star_formation_master_girl":
		return true
	default:
		return false
	}
}
