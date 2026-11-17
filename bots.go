package leverprice

import (
	"regexp"
	"strings"
)

// botPatterns mirror the reference SDK except (?<!cu)bot(?![a-z]), which needs lookaround that
// RE2 lacks and is implemented by hasBotWord.
var botPatterns = []string{
	"crawl",
	"spider",
	"scrap",
	"slurp",
	"archiver",
	"fetcher",
	`\+https?:`,
	"facebookexternalhit",
	"facebookcatalog",
	"meta-external",
	"whatsapp/",
	"skypeuripreview",
	"bingpreview",
	"microsoftpreview",
	"link ?preview",
	"url preview",
	"web preview",
	"slack-imgproxy",
	"vkshare",
	"embedly",
	"iframely",
	"cardyb",
	"outbrain",
	"flipboardproxy",
	"mediapartners-google",
	"apis-google",
	"duplexweb-google",
	"google-(?:inspectiontool|site-verification|read-aloud|pagerenderer|structured-data|safety)",
	"googleother",
	"googleproducer",
	"google favicon",
	"chatgpt-user",
	"claude-user",
	"perplexity-user",
	"mistralai-user",
	"anthropic-ai",
	"cohere-ai",
	"daumoa",
	"ahrefs",
	"sitebulb",
	"siteimprove",
	"barkrowler",
	"botify",
	"seokicks",
	"validator",
	"checklink",
	"heritrix",
	"headless",
	"phantomjs",
	"slimerjs",
	"htmlunit",
	"jsdom",
	"cypress",
	"lighthouse",
	"page ?speed",
	"gtmetrix",
	"ptst/",
	"pingdom",
	"uptime",
	"statuscake",
	"site24x7",
	"newrelicpinger",
	"synthetic",
	"checkly",
	"health-?check",
	"googlehc",
	"kube-probe",
	"nagios",
	"zabbix",
	"catchpoint",
	"freshping",
	"hetrixtools",
	"ohdear",
	"monitoring",
	"curl",
	"wget",
	"httpie",
	"python-requests",
	"python-urllib",
	"python-httpx",
	"aiohttp",
	"go-http-client",
	"^java/",
	"apache-httpclient",
	"libwww-perl",
	"lwp::",
	"guzzlehttp",
	"postmanruntime",
	"insomnia",
	"axios/",
	"node-fetch",
	"undici",
}

var botRE = regexp.MustCompile(strings.Join(botPatterns, "|"))

func hasBotWord(ua string) bool {
	for i := 0; ; {
		j := strings.Index(ua[i:], "bot")
		if j < 0 {
			return false
		}
		at, end := i+j, i+j+3
		letterAfter := end < len(ua) && ua[end] >= 'a' && ua[end] <= 'z'
		cuBefore := at >= 2 && ua[at-2:at] == "cu"
		if !letterAfter && !cuBefore {
			return true
		}
		i = at + 1
	}
}

// IsBot reports whether userAgent belongs to a crawler, link previewer, headless browser,
// monitor or command-line client. An empty user agent is not a bot.
func IsBot(userAgent string) bool {
	ua := jsTrim(userAgent)
	if ua == "" {
		return false
	}
	ua = asciiLower(ua)
	return hasBotWord(ua) || botRE.MatchString(ua)
}
