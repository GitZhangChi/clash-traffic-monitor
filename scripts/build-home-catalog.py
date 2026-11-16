#!/usr/bin/env python3
"""Build the bundled service catalogue from a pinned domain-list-community checkout."""
import argparse
import json
import subprocess
from pathlib import Path

SERVICES = [
    ("youtube", "YouTube", "youtube"),
    ("netflix", "Netflix", "netflix"),
    ("disney", "Disney+", "disney"),
    ("primevideo", "Prime Video", "primevideo"),
    ("apple-tvplus", "Apple TV+", "apple-tvplus"),
    ("hulu", "Hulu", "hulu"),
    ("plex", "Plex", "plex"),
    ("tmdb", "TMDB 影视资料", "tmdb"),
    ("bilibili", "哔哩哔哩", "bilibili"),
    ("douyin", "抖音", "douyin"),
    ("tiktok", "TikTok", "tiktok"),
    ("kuaishou", "快手", "kuaishou"),
    ("xiaohongshu", "小红书", "xiaohongshu"),
    ("iqiyi", "爱奇艺", "iqiyi"),
    ("youku", "优酷", "youku"),
    ("spotify", "Spotify", "spotify"),
    ("telegram", "Telegram", "telegram"),
    ("twitter", "X / Twitter", "twitter"),
    ("slack", "Slack", "slack"),
    ("openai", "OpenAI / ChatGPT", "openai"),
    ("anthropic", "Anthropic / Claude", "anthropic"),
    ("google-gemini", "Google Gemini", "google-gemini"),
    ("xai", "xAI / Grok", "xai"),
    ("github", "GitHub", "github"),
    ("binance", "币安", "binance"),
    ("okx", "OKX", "okx"),
    ("steam", "Steam", "steam"),
    ("playstation", "PlayStation", "playstation"),
    ("nintendo", "Nintendo", "nintendo"),
    ("dingtalk", "钉钉", "dingtalk"),
    ("zoom", "Zoom", "zoom"),
    ("pinduoduo", "拼多多", "pinduoduo"),
    ("jd", "京东", "jd"),
    ("narwal", "云鲸设备", "narwal"),
    ("xiaomi-iot", "米家设备", "xiaomi-iot"),
    ("icloud", "iCloud", "icloud"),
    ("google", "Google 服务", "google"),
    ("apple", "Apple 服务", "apple"),
    ("microsoft", "Microsoft 服务", "microsoft"),
    ("tencent", "腾讯服务", "tencent"),
    ("sina", "新浪 / 微博", "sina"),
    ("xiaomi", "小米服务", "xiaomi"),
    ("samsung", "Samsung 服务", "samsung"),
    ("cloudflare", "Cloudflare 服务", "cloudflare"),
]


def read_entries(root, name, stack=()):
    if name in stack:
        raise ValueError(f"cyclic include: {' -> '.join((*stack, name))}")
    entries = []
    for raw in (root / "data" / name).read_text().splitlines():
        terms = raw.split("#", 1)[0].split()
        if not terms:
            continue
        token, attributes = terms[0], set(terms[1:])
        kind, separator, value = token.partition(":")
        if not separator:
            kind, value = "domain", token
        if kind == "include":
            included = read_entries(root, value, (*stack, name))
            for entry in included:
                include = True
                for attribute in attributes:
                    if attribute.startswith("@!"):
                        include &= "@" + attribute[2:] not in entry[2]
                    elif attribute.startswith("@"):
                        include &= attribute in entry[2]
                if include:
                    entries.append(entry)
        else:
            entries.append((kind, value, attributes))
    return entries


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--output", type=Path, default=Path("home-applications.json"))
    args = parser.parse_args()
    revision = subprocess.check_output(["git", "-C", str(args.source), "rev-parse", "HEAD"], text=True).strip()
    services = [{"id": "wechat", "name": "微信", "suffixes": ["wechat.com", "weixin.com", "servicewechat.com", "weixin.qq.com", "wx.qq.com", "weixinbridge.com"]}]
    fields = {"domain": "suffixes", "full": "domains", "keyword": "keywords", "regexp": "regex"}
    for app_id, name, source in SERVICES:
        entry = {"id": app_id, "name": name, "ruleSets": ["github-" + app_id]}
        for kind, value, _ in read_entries(args.source, source):
            if kind in fields:
                entry.setdefault(fields[kind], set()).add(value)
            else:
                raise ValueError(f"unsupported rule kind {kind}: {source}")
        services.append({key: sorted(value) if isinstance(value, set) else value for key, value in entry.items()})
    document = {"source": "https://github.com/v2fly/domain-list-community", "revision": revision, "applications": services}
    args.output.write_text(json.dumps(document, ensure_ascii=False, indent=2) + "\n")
    licenses = args.output.parent / "THIRD_PARTY_LICENSES"
    licenses.mkdir(exist_ok=True)
    license_text = subprocess.check_output(["git", "-C", str(args.source), "show", "HEAD:LICENSE"], text=True)
    (licenses / "domain-list-community.txt").write_text(license_text)
    print(json.dumps({"services": len(services), "rules": sum(len(value) for app in services for key, value in app.items() if key in fields.values()), "source_revision": revision}))


if __name__ == "__main__":
    main()
