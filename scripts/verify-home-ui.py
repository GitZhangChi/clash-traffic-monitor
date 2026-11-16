#!/usr/bin/env python3
"""Exercise the real local home dashboard in desktop and mobile Chromium."""
import argparse
import json
import time
from pathlib import Path
from urllib.parse import urlencode

from playwright.sync_api import sync_playwright, expect
from home_runtime_import import load_runtime_settings


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", default="http://127.0.0.1:18185")
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--artifacts", type=Path, required=True)
    parser.add_argument("--browser-executable", type=Path)
    args = parser.parse_args()
    args.artifacts.mkdir(parents=True, exist_ok=True)
    secret = load_runtime_settings(args.config)["secret"]
    errors = []
    with sync_playwright() as p:
        launch_options = {"headless": True}
        if args.browser_executable:
            launch_options["executable_path"] = str(args.browser_executable)
        browser = p.chromium.launch(**launch_options)
        context = browser.new_context(viewport={"width": 1440, "height": 1080}, device_scale_factor=1)
        page = context.new_page()
        page.on("pageerror", lambda error: errors.append(str(error)))
        page.goto(args.url, wait_until="networkidle")
        expect(page.locator("h1")).to_have_text("全屋流量")
        page.locator("#token").fill(secret)
        page.get_by_role("button", name="进入监控").click()
        page.wait_for_load_state("networkidle")
        page.wait_for_function("document.getElementById('status').textContent === '正在采集'")
        page.wait_for_function("document.querySelectorAll('#devices .device-button').length > 0")
        now = int(time.time() * 1000)
        query = urlencode({"start": now - 120000, "end": now + 60000})
        response = context.request.get(args.url + "/api/home/summary?" + query)
        assert response.ok
        data = response.json()
        assert data["lastInventory"] > 0
        merged = [d for d in data["devices"] if any(":" not in a for a in d["addresses"]) and any(":" in a and not a.startswith("fe80:") for a in d["addresses"])]
        assert merged, "No IPv4/IPv6 device merge was observed"
        apple_tv = next((d for d in data["devices"] if "apple-tv" in d["name"].lower()), None)
        assert apple_tv, "Apple TV from the known household inventory is missing"
        page.get_by_role("button", name=apple_tv["name"], exact=True).click()
        expect(page.locator("#selectedName")).to_contain_text(apple_tv["name"])
        expect(page.locator("#totalDetail")).to_have_text("设备用量按连接采样统计")
        page.get_by_role("button", name="全屋设备", exact=True).click()
        expect(page.locator("#selectedName")).to_have_text("")
        page.get_by_role("button", name="昨天", exact=True).click()
        page.wait_for_load_state("networkidle")
        expect(page.locator("#total")).to_have_text("0 B")
        page.get_by_role("button", name="近 1 小时", exact=True).click()
        page.wait_for_function("document.querySelectorAll('#applications .app-button').length > 0", timeout=30000)
        page.locator("#applications .app-button").first.click()
        expect(page.locator("#clearApp")).to_be_visible()
        page.locator("#clearApp").click()
        expect(page.locator("#clearApp")).to_be_hidden()
        page.screenshot(path=str(args.artifacts / "desktop.png"), full_page=True)
        page.set_viewport_size({"width": 390, "height": 844})
        assert page.evaluate("document.documentElement.scrollWidth <= window.innerWidth"), "Mobile page overflows horizontally"
        assert page.locator(".devices-panel .table-wrap").evaluate("element => element.scrollWidth <= element.clientWidth"), "Mobile device usage columns are clipped"
        page.screenshot(path=str(args.artifacts / "mobile.png"), full_page=True)
        page.locator(".apps-panel").scroll_into_view_if_needed()
        page.screenshot(path=str(args.artifacts / "mobile-services.png"))
        assert not errors, errors
        final = context.request.get(args.url + "/api/home/summary?" + query).json()
        report = {"ok": True, "devices": len([d for d in final["devices"] if d.get("mac")]), "mergedDevices": len([d for d in merged if d.get("mac")]),
                  "services": len(final["applications"]), "observedBytes": final["totals"]["total"],
                  "checks": ["login", "live collection", "IPv4/IPv6 merge", "Apple TV drilldown", "historical range", "service filter", "mobile layout"],
                  "pageErrors": errors}
        (args.artifacts / "verification.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
        print(json.dumps(report, ensure_ascii=False))
        browser.close()


if __name__ == "__main__":
    main()
