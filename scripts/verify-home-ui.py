#!/usr/bin/env python3
"""Exercise a running home dashboard against its actual retained data."""
import argparse
import json
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
    args.artifacts.chmod(0o700)
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
        page.locator("#token").fill(secret)
        page.get_by_role("button", name="进入监控").click()
        page.wait_for_function("document.body.dataset.renderedRequest")
        expect(page.locator("#status")).to_have_text("正在采集")

        def act(action):
            before = int(page.locator("body").get_attribute("data-rendered-request"))
            action()
            page.wait_for_function("before => Number(document.body.dataset.renderedRequest) > before", arg=before)

        def summary(**params):
            data = page.evaluate("({start:state.data.start,end:state.data.end,device:state.device})")
            data.update(params)
            response = context.request.get(args.url + "/api/home/summary?" + urlencode(data))
            assert response.ok, response.status
            return response.json()

        data = summary()
        assert data["lastInventory"] > 0
        merged = [d for d in data["devices"] if d.get("mac") and any(":" not in a for a in d["addresses"]) and any(":" in a and not a.startswith("fe80:") for a in d["addresses"])]
        assert merged, "No IPv4/IPv6 device merge observed"
        visible = next(d for d in data["devices"] if d.get("mac") and d["total"] > 0)
        act(lambda: page.get_by_role("button", name=visible["name"], exact=True).click())
        expect(page.locator("#selectedName")).to_contain_text(visible["name"])
        expect(page.locator("#totalDetail")).to_have_text("设备用量按连接采样统计")
        act(lambda: page.get_by_role("button", name="全屋设备", exact=True).click())
        expect(page.locator("#selectedName")).to_have_text("")

        act(lambda: page.get_by_role("button", name="昨天", exact=True).click())
        yesterday = summary()
        shown = page.evaluate("state.data.totals.total")
        assert shown == yesterday["totals"]["total"], "Historical page differs from retained data"
        act(lambda: page.get_by_role("button", name="今天", exact=True).click())
        data = summary()
        for app in sorted(data["applications"], key=lambda a: a["total"])[:1] + [a for a in data["applications"] if a["id"] == "telegram"]:
            act(lambda: page.locator(f'[data-application="{app["id"]}"]').click())
            filtered = summary(application=app["id"], limit=50)
            assert filtered["domainCount"] > 0, f"No details for {app['id']}"
            current_app = next(a for a in filtered["applications"] if a["id"] == app["id"])
            assert filtered["domainUsage"]["total"] == current_app["total"], "Domain/application totals differ"
            assert all(d["applicationId"] == app["id"] for d in filtered["domains"])
            expect(page.locator("#domains .empty")).to_have_count(0)
            act(lambda: page.locator("#clearApp").click())

        if data["domainCount"] > 50:
            act(lambda: page.locator("#domainNext").click())
            assert page.evaluate("state.data.domainOffset") == 50
            act(lambda: page.locator("#domainPrevious").click())
            assert page.evaluate("state.data.domainOffset") == 0
        zero = sum(d["total"] == 0 for d in data["devices"])
        before = page.locator("#devices .device-button").count()
        page.locator("#showInactive").check()
        assert page.locator("#devices .device-button").count() == before + zero
        page.locator("#showInactive").uncheck()
        page.locator(".applications").evaluate("e => e.scrollTop = 0")
        page.evaluate("window.scrollTo(0, 0)")
        page.screenshot(path=str(args.artifacts / "desktop.png"), full_page=True)
        page.set_viewport_size({"width": 390, "height": 844})
        assert page.evaluate("document.documentElement.scrollWidth <= window.innerWidth"), "Mobile page overflows"
        assert page.locator(".devices-panel .table-wrap").evaluate("e => e.scrollWidth <= e.clientWidth"), "Device columns clipped"
        assert page.locator(".domains-panel .table-wrap").evaluate("e => e.scrollWidth <= e.clientWidth"), "Domain columns clipped"
        page.screenshot(path=str(args.artifacts / "mobile.png"), full_page=True)
        page.locator(".domains-panel").scroll_into_view_if_needed()
        page.screenshot(path=str(args.artifacts / "mobile-domains.png"))
        assert not errors, errors
        report = {"ok": True, "devices": len([d for d in data["devices"] if d.get("mac")]), "mergedDevices": len(merged),
                  "services": len(data["applications"]), "domainCount": data["domainCount"], "yesterdayBytes": yesterday["totals"]["total"],
                  "checks": ["login", "live collection", "IPv4/IPv6 merge", "device drilldown", "retained history", "small-service details", "Telegram totals", "pagination", "zero-usage toggle", "mobile layout"],
                  "pageErrors": errors}
        (args.artifacts / "verification.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
        print(json.dumps(report, ensure_ascii=False))
        browser.close()


if __name__ == "__main__":
    main()
