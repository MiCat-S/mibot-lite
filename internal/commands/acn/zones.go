package acn

import (
	"strconv"
	"strings"
	"time"
)

// 下面三张表原样取自 MiBox 的 autochangename（v1、v2 两版内容相同），
// 供 simp 格式在时区数据库给不出字母缩写时查用。

// zoneAbbreviations 按地区给出缩写，不区分冬夏令时。
var zoneAbbreviations = map[string]string{
	"Africa/Abidjan": "GMT", "Africa/Accra": "GMT", "Africa/Addis_Ababa": "EAT", "Africa/Algiers": "CET",
	"Africa/Asmara": "EAT", "Africa/Bamako": "GMT", "Africa/Bangui": "WAT", "Africa/Banjul": "GMT",
	"Africa/Bissau": "GMT", "Africa/Blantyre": "CAT", "Africa/Brazzaville": "WAT", "Africa/Cairo": "EET",
	"Africa/Casablanca": "WET", "Africa/Ceuta": "CET", "Africa/Dakar": "GMT", "Africa/Dar_es_Salaam": "EAT",
	"Africa/Djibouti": "EAT", "Africa/Douala": "WAT", "Africa/El_Aaiun": "WET", "Africa/Freetown": "GMT",
	"Africa/Gaborone": "CAT", "Africa/Harare": "CAT", "Africa/Johannesburg": "SAST", "Africa/Juba": "CAT",
	"Africa/Kampala": "EAT", "Africa/Khartoum": "CAT", "Africa/Kigali": "CAT", "Africa/Kinshasa": "WAT",
	"Africa/Lagos": "WAT", "Africa/Libreville": "WAT", "Africa/Lome": "GMT", "Africa/Luanda": "WAT",
	"Africa/Lubumbashi": "CAT", "Africa/Lusaka": "CAT", "Africa/Malabo": "WAT", "Africa/Maputo": "CAT",
	"Africa/Maseru": "SAST", "Africa/Mbabane": "SAST", "Africa/Mogadishu": "EAT", "Africa/Monrovia": "GMT",
	"Africa/Nairobi": "EAT", "Africa/Ndjamena": "WAT", "Africa/Niamey": "WAT", "Africa/Nouakchott": "GMT",
	"Africa/Ouagadougou": "GMT", "Africa/Porto-Novo": "WAT", "Africa/Sao_Tome": "GMT", "Africa/Tripoli": "EET",
	"Africa/Tunis": "CET", "Africa/Windhoek": "CAT", "America/Adak": "HST", "America/Anchorage": "AKST",
	"America/Anguilla": "AST", "America/Antigua": "AST", "America/Araguaina": "BRT", "America/Argentina/Buenos_Aires": "ART",
	"America/Argentina/Catamarca": "ART", "America/Argentina/Cordoba": "ART", "America/Argentina/Mendoza": "ART", "America/Aruba": "AST",
	"America/Asuncion": "PYT", "America/Atikokan": "EST", "America/Bahia": "BRT", "America/Bahia_Banderas": "CST",
	"America/Barbados": "AST", "America/Belem": "BRT", "America/Belize": "CST", "America/Blanc-Sablon": "AST",
	"America/Boa_Vista": "AMT", "America/Bogota": "COT", "America/Boise": "MST", "America/Cambridge_Bay": "MST",
	"America/Campo_Grande": "AMT", "America/Cancun": "EST", "America/Caracas": "VET", "America/Cayenne": "GFT",
	"America/Cayman": "EST", "America/Chicago": "CST", "America/Chihuahua": "MST", "America/Ciudad_Juarez": "MST",
	"America/Costa_Rica": "CST", "America/Creston": "MST", "America/Cuiaba": "AMT", "America/Curacao": "AST",
	"America/Danmarkshavn": "GMT", "America/Dawson": "MST", "America/Dawson_Creek": "MST", "America/Denver": "MST",
	"America/Detroit": "EST", "America/Dominica": "AST", "America/Edmonton": "MST", "America/Eirunepe": "ACT",
	"America/El_Salvador": "CST", "America/Fort_Nelson": "MST", "America/Fortaleza": "BRT", "America/Glace_Bay": "AST",
	"America/Goose_Bay": "AST", "America/Grand_Turk": "EST", "America/Guatemala": "CST", "America/Guayaquil": "ECT",
	"America/Guyana": "GYT", "America/Halifax": "AST", "America/Havana": "CST", "America/Hermosillo": "MST",
	"America/Indiana/Indianapolis": "EST", "America/Indiana/Knox": "CST", "America/Indiana/Marengo": "EST", "America/Indiana/Petersburg": "EST",
	"America/Indiana/Tell_City": "CST", "America/Indiana/Vevay": "EST", "America/Indiana/Vincennes": "EST", "America/Indiana/Winamac": "EST",
	"America/Inuvik": "MST", "America/Iqaluit": "EST", "America/Jamaica": "EST", "America/Juneau": "AKST",
	"America/Kentucky/Louisville": "EST", "America/Kentucky/Monticello": "EST", "America/Kralendijk": "AST", "America/La_Paz": "BOT",
	"America/Lima": "PET", "America/Los_Angeles": "PST", "America/Maceio": "BRT", "America/Managua": "CST",
	"America/Manaus": "AMT", "America/Martinique": "AST", "America/Matamoros": "CST", "America/Mazatlan": "MST",
	"America/Menominee": "CST", "America/Merida": "CST", "America/Metlakatla": "AKST", "America/Mexico_City": "CST",
	"America/Miquelon": "PMST", "America/Moncton": "AST", "America/Monterrey": "CST", "America/Montevideo": "UYT",
	"America/Nassau": "EST", "America/New_York": "EST", "America/Nipigon": "EST", "America/Nome": "AKST",
	"America/Noronha": "FNT", "America/North_Dakota/Beulah": "CST", "America/North_Dakota/Center": "CST", "America/North_Dakota/New_Salem": "CST",
	"America/Nuuk": "WGT", "America/Ojinaga": "CST", "America/Panama": "EST", "America/Paramaribo": "SRT",
	"America/Phoenix": "MST", "America/Port_of_Spain": "AST", "America/Port-au-Prince": "EST", "America/Porto_Velho": "AMT",
	"America/Puerto_Rico": "AST", "America/Punta_Arenas": "CLT", "America/Rainy_River": "CST", "America/Rankin_Inlet": "CST",
	"America/Recife": "BRT", "America/Regina": "CST", "America/Resolute": "CST", "America/Rio_Branco": "ACT",
	"America/Santarem": "BRT", "America/Santiago": "CLT", "America/Santo_Domingo": "AST", "America/Sao_Paulo": "BRT",
	"America/Scoresbysund": "EGT", "America/Sitka": "AKST", "America/St_Johns": "NST", "America/Swift_Current": "CST",
	"America/Tegucigalpa": "CST", "America/Thule": "AST", "America/Thunder_Bay": "EST", "America/Tijuana": "PST",
	"America/Toronto": "EST", "America/Tortola": "AST", "America/Vancouver": "PST", "America/Whitehorse": "MST",
	"America/Winnipeg": "CST", "America/Yakutat": "AKST", "America/Yellowknife": "MST", "Antarctica/Casey": "AWST",
	"Antarctica/Davis": "DAVT", "Antarctica/DumontDUrville": "DDUT", "Antarctica/Macquarie": "AEST", "Antarctica/Mawson": "MAWT",
	"Antarctica/McMurdo": "NZST", "Antarctica/Palmer": "CLT", "Antarctica/Rothera": "ROT", "Antarctica/South_Pole": "NZST",
	"Antarctica/Syowa": "SYOT", "Antarctica/Troll": "UTC", "Antarctica/Vostok": "VOST", "Asia/Aden": "AST",
	"Asia/Almaty": "ALMT", "Asia/Amman": "EET", "Asia/Anadyr": "ANAT", "Asia/Aqtau": "AQTT",
	"Asia/Aqtobe": "AQTT", "Asia/Ashgabat": "TMT", "Asia/Atyrau": "AQTT", "Asia/Baghdad": "AST",
	"Asia/Bahrain": "AST", "Asia/Baku": "AZT", "Asia/Bangkok": "ICT", "Asia/Barnaul": "KRAT",
	"Asia/Beirut": "EET", "Asia/Bishkek": "KGT", "Asia/Brunei": "BNT", "Asia/Calcutta": "IST",
	"Asia/Chita": "YAKT", "Asia/Choibalsan": "CHOT", "Asia/Chongqing": "CST", "Asia/Colombo": "IST",
	"Asia/Damascus": "EET", "Asia/Dhaka": "BDT", "Asia/Dili": "TLT", "Asia/Dubai": "GST",
	"Asia/Dushanbe": "TJT", "Asia/Famagusta": "EET", "Asia/Gaza": "EET", "Asia/Harbin": "CST",
	"Asia/Hebron": "EET", "Asia/Ho_Chi_Minh": "ICT", "Asia/Hong_Kong": "HKT", "Asia/Hovd": "HOVT",
	"Asia/Irkutsk": "IRKT", "Asia/Istanbul": "TRT", "Asia/Jakarta": "WIB", "Asia/Jayapura": "WIT",
	"Asia/Jerusalem": "IST", "Asia/Kabul": "AFT", "Asia/Kamchatka": "PETT", "Asia/Karachi": "PKT",
	"Asia/Kashgar": "XJT", "Asia/Kathmandu": "NPT", "Asia/Khandyga": "YAKT", "Asia/Kolkata": "IST",
	"Asia/Krasnoyarsk": "KRAT", "Asia/Kuala_Lumpur": "MYT", "Asia/Kuching": "MYT", "Asia/Kuwait": "AST",
	"Asia/Macao": "CST", "Asia/Magadan": "MAGT", "Asia/Makassar": "WITA", "Asia/Manila": "PST",
	"Asia/Muscat": "GST", "Asia/Nicosia": "EET", "Asia/Novokuznetsk": "KRAT", "Asia/Novosibirsk": "NOVT",
	"Asia/Omsk": "OMST", "Asia/Oral": "ORAT", "Asia/Phnom_Penh": "ICT", "Asia/Pontianak": "WIB",
	"Asia/Pyongyang": "KST", "Asia/Qatar": "AST", "Asia/Qostanay": "QYZT", "Asia/Qyzylorda": "QYZT",
	"Asia/Rangoon": "MMT", "Asia/Riyadh": "AST", "Asia/Sakhalin": "SAKT", "Asia/Samarkand": "UZT",
	"Asia/Seoul": "KST", "Asia/Shanghai": "CST", "Asia/Singapore": "SGT", "Asia/Srednekolymsk": "SRET",
	"Asia/Taipei": "CST", "Asia/Tashkent": "UZT", "Asia/Tbilisi": "GET", "Asia/Tehran": "IRST",
	"Asia/Tel_Aviv": "IST", "Asia/Thimphu": "BTT", "Asia/Tokyo": "JST", "Asia/Tomsk": "TOMT",
	"Asia/Ulaanbaatar": "ULAT", "Asia/Urumqi": "XJT", "Asia/Ust-Nera": "VLAT", "Asia/Vientiane": "ICT",
	"Asia/Vladivostok": "VLAT", "Asia/Yakutsk": "YAKT", "Asia/Yangon": "MMT", "Asia/Yekaterinburg": "YEKT",
	"Asia/Yerevan": "AMT", "Atlantic/Azores": "AZOT", "Atlantic/Bermuda": "AST", "Atlantic/Canary": "WET",
	"Atlantic/Cape_Verde": "CVT", "Atlantic/Faeroe": "WET", "Atlantic/Faroe": "WET", "Atlantic/Jan_Mayen": "CET",
	"Atlantic/Madeira": "WET", "Atlantic/Reykjavik": "GMT", "Atlantic/South_Georgia": "GST", "Atlantic/St_Helena": "GMT",
	"Atlantic/Stanley": "FKST", "Arctic/Longyearbyen": "CET", "Australia/ACT": "AEST", "Australia/Adelaide": "ACST",
	"Australia/Brisbane": "AEST", "Australia/Broken_Hill": "ACST", "Australia/Canberra": "AEST", "Australia/Currie": "AEST",
	"Australia/Darwin": "ACST", "Australia/Eucla": "ACWST", "Australia/Hobart": "AEST", "Australia/LHI": "LHST",
	"Australia/Lindeman": "AEST", "Australia/Lord_Howe": "LHST", "Australia/Melbourne": "AEST", "Australia/North": "ACST",
	"Australia/NSW": "AEST", "Australia/Perth": "AWST", "Australia/Queensland": "AEST", "Australia/South": "ACST",
	"Australia/Sydney": "AEST", "Australia/Tasmania": "AEST", "Australia/Victoria": "AEST", "Australia/West": "AWST",
	"Australia/Yancowinna": "ACST", "Europe/Andorra": "CET", "Europe/Astrakhan": "SAMT", "Europe/Athens": "EET",
	"Europe/Belgrade": "CET", "Europe/Berlin": "CET", "Europe/Brussels": "CET", "Europe/Bucharest": "EET",
	"Europe/Budapest": "CET", "Europe/Chisinau": "EET", "Europe/Dublin": "GMT", "Europe/Gibraltar": "CET",
	"Europe/Helsinki": "EET", "Europe/Istanbul": "TRT", "Europe/Kaliningrad": "EET", "Europe/Kirov": "MSK",
	"Europe/Kyiv": "EET", "Europe/Lisbon": "WET", "Europe/London": "GMT", "Europe/Madrid": "CET",
	"Europe/Malta": "CET", "Europe/Minsk": "MSK", "Europe/Moscow": "MSK", "Europe/Paris": "CET",
	"Europe/Prague": "CET", "Europe/Riga": "EET", "Europe/Rome": "CET", "Europe/Samara": "SAMT",
	"Europe/Saratov": "SAMT", "Europe/Simferopol": "MSK", "Europe/Sofia": "EET", "Europe/Tallinn": "EET",
	"Europe/Tirane": "CET", "Europe/Ulyanovsk": "SAMT", "Europe/Vienna": "CET", "Europe/Vilnius": "EET",
	"Europe/Volgograd": "MSK", "Europe/Warsaw": "CET", "Europe/Zurich": "CET", "Indian/Chagos": "IOT",
	"Indian/Christmas": "CXT", "Indian/Cocos": "CCT", "Indian/Kerguelen": "TFT", "Indian/Maldives": "MVT",
	"Indian/Mauritius": "MUT", "Indian/Mayotte": "EAT", "Indian/Reunion": "RET", "Pacific/Apia": "WST",
	"Pacific/Auckland": "NZST", "Pacific/Bougainville": "BST", "Pacific/Chatham": "CHAST", "Pacific/Easter": "EASST",
	"Pacific/Efate": "VUT", "Pacific/Fakaofo": "TKT", "Pacific/Fiji": "FJT", "Pacific/Galapagos": "GALT",
	"Pacific/Gambier": "GAMT", "Pacific/Guadalcanal": "SBT", "Pacific/Guam": "ChST", "Pacific/Honolulu": "HST",
	"Pacific/Kanton": "PHOT", "Pacific/Kiritimati": "LINT", "Pacific/Kosrae": "KOST", "Pacific/Kwajalein": "MHT",
	"Pacific/Marquesas": "MART", "Pacific/Nauru": "NRT", "Pacific/Niue": "NUT", "Pacific/Norfolk": "NFT",
	"Pacific/Noumea": "NCT", "Pacific/Pago_Pago": "SST", "Pacific/Palau": "PWT", "Pacific/Pitcairn": "PST",
	"Pacific/Port_Moresby": "PGT", "Pacific/Rarotonga": "CKT", "Pacific/Tahiti": "TAHT", "Pacific/Tarawa": "GILT",
	"Pacific/Tongatapu": "TOT", "Pacific/Wake": "WAKT", "Pacific/Wallis": "WFT", "UTC": "UTC",
	"Etc/UTC": "UTC", "Etc/GMT": "GMT",
}

// offsetAbbreviations 按 UTC 偏移量给出缩写，地区表里也查不到时才用。
var offsetAbbreviations = map[string]string{
	"+00:00": "GMT", "+01:00": "CET", "+02:00": "EET", "+03:00": "MSK", "+04:00": "GST",
	"+05:00": "PKT", "+05:30": "IST", "+05:45": "NPT", "+06:00": "BST", "+06:30": "MMT",
	"+07:00": "ICT", "+08:00": "CST", "+08:45": "ACWST", "+09:00": "JST", "+09:30": "ACST",
	"+10:00": "AEST", "+10:30": "LHST", "+11:00": "AEDT", "+12:00": "NZST", "+13:00": "NZDT",
	"+14:00": "LINT", "-01:00": "AZOT", "-02:00": "GST", "-03:00": "ART", "-03:30": "NST",
	"-04:00": "AST", "-05:00": "EST", "-06:00": "CST", "-07:00": "MST", "-08:00": "PST",
	"-09:00": "AKST", "-10:00": "HST", "-11:00": "SST", "-12:00": "AoE",
}

// seasonalAbbreviations 给有冬夏令时的地区分别记下两种缩写：[标准时间, 夏令时]。
var seasonalAbbreviations = map[string][2]string{
	"America/Anchorage":   {"AKST", "AKDT"},
	"America/Chicago":     {"CST", "CDT"},
	"America/Denver":      {"MST", "MDT"},
	"America/Detroit":     {"EST", "EDT"},
	"America/Halifax":     {"AST", "ADT"},
	"America/Los_Angeles": {"PST", "PDT"},
	"America/New_York":    {"EST", "EDT"},
	"America/Santiago":    {"CLT", "CLST"},
	"America/St_Johns":    {"NST", "NDT"},
	"America/Toronto":     {"EST", "EDT"},
	"America/Vancouver":   {"PST", "PDT"},
	"America/Winnipeg":    {"CST", "CDT"},
	"Atlantic/Azores":     {"AZOT", "AZOST"},
	"Atlantic/Bermuda":    {"AST", "ADT"},
	"Australia/Adelaide":  {"ACST", "ACDT"},
	"Australia/Hobart":    {"AEST", "AEDT"},
	"Australia/Melbourne": {"AEST", "AEDT"},
	"Australia/Sydney":    {"AEST", "AEDT"},
	"Europe/Athens":       {"EET", "EEST"},
	"Europe/Berlin":       {"CET", "CEST"},
	"Europe/Brussels":     {"CET", "CEST"},
	"Europe/Dublin":       {"GMT", "IST"},
	"Europe/Helsinki":     {"EET", "EEST"},
	"Europe/Lisbon":       {"WET", "WEST"},
	"Europe/London":       {"GMT", "BST"},
	"Europe/Madrid":       {"CET", "CEST"},
	"Europe/Paris":        {"CET", "CEST"},
	"Europe/Prague":       {"CET", "CEST"},
	"Europe/Rome":         {"CET", "CEST"},
	"Europe/Vienna":       {"CET", "CEST"},
	"Europe/Warsaw":       {"CET", "CEST"},
	"Europe/Zurich":       {"CET", "CEST"},
	"Pacific/Auckland":    {"NZST", "NZDT"},
	"Pacific/Chatham":     {"CHAST", "CHADT"},
}

// zoneLabel 按所选的格式显示时区在 now 这一刻的标识。
func zoneLabel(zone, format string, now time.Time) string {
	location, err := time.LoadLocation(zone)
	if err != nil {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(format), "custom:") {
		return format[len("custom:"):]
	}
	_, offset := now.In(location).Zone()
	sign, hours, minutes := splitOffset(offset)
	switch strings.ToUpper(strings.TrimSpace(format)) {
	case "SIMP":
		return zoneAbbreviation(zone, location, now)
	case "OFFSET":
		return offsetKey(offset)
	case "UTC":
		if hours == 0 && minutes == 0 {
			return "UTC"
		}
		if minutes != 0 {
			return "UTC" + offsetKey(offset)
		}
		return "UTC" + sign + strconv.Itoa(hours)
	}
	if hours == 0 && minutes == 0 {
		return "GMT"
	}
	if minutes != 0 {
		return "GMT" + offsetKey(offset)
	}
	return "GMT" + sign + strconv.Itoa(hours)
}

// zoneAbbreviation 给出时区缩写，查找顺序与 MiBox 相同。
//
// Go 的时区数据库对多数地区记有字母缩写（CST、JST、EDT），遇到夏令时切换也会跟着变；
// 但 Asia/Singapore、Asia/Dubai、Asia/Bangkok 这类地区只记了 "+08"、"+04"、"+07"。
// 这时依次查冬夏令时表、地区表、偏移量表，都查不到就写成 GMT+08:00 这样的形式。
func zoneAbbreviation(zone string, location *time.Location, now time.Time) string {
	name, offset := now.In(location).Zone()
	if name != "" && !strings.HasPrefix(name, "+") && !strings.HasPrefix(name, "-") {
		return name
	}
	if pair, ok := seasonalAbbreviations[zone]; ok {
		year := now.UTC().Year()
		_, january := time.Date(year, time.January, 1, 12, 0, 0, 0, time.UTC).In(location).Zone()
		_, july := time.Date(year, time.July, 1, 12, 0, 0, 0, time.UTC).In(location).Zone()
		if january != july {
			if offset == max(january, july) {
				return pair[1]
			}
			return pair[0]
		}
	}
	if exact, ok := zoneAbbreviations[zone]; ok {
		return exact
	}
	key := offsetKey(offset)
	if fallback, ok := offsetAbbreviations[key]; ok {
		return fallback
	}
	return "GMT" + key
}

// splitOffset 把以秒计的 UTC 偏移拆成符号、小时和分钟。
func splitOffset(offset int) (string, int, int) {
	sign := "+"
	if offset < 0 {
		sign, offset = "-", -offset
	}
	return sign, offset / 3600, (offset % 3600) / 60
}

// offsetKey 把偏移写成 +08:00 的形式，也是 offsetAbbreviations 的键。
func offsetKey(offset int) string {
	sign, hours, minutes := splitOffset(offset)
	return sign + pad2(hours) + ":" + pad2(minutes)
}

func pad2(value int) string {
	if value < 10 {
		return "0" + strconv.Itoa(value)
	}
	return strconv.Itoa(value)
}
