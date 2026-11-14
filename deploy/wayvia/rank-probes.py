#!/usr/bin/env python3
"""在实际出口主机上核验三个公共回显服务，输出可用于 probe_urls 的优先顺序。"""

import argparse
import datetime
import ipaddress
import json
import statistics
import subprocess

URLS = (
    "https://api6.ipify.org",
    "https://ipv6.icanhazip.com",
    "https://v6.ident.me",
)


def sample(url: str, source: str) -> dict:
    result = subprocess.run(
        [
            "curl", "--silent", "--show-error", "--noproxy", "*", "--http1.1",
            "-6", "--interface", source, "--connect-timeout", "3", "--max-time", "5",
            "--max-filesize", "256", "-A", "Go-http-client/1.1",
            "--write-out", "\n%{http_code}\n%{time_total}\n", url,
        ],
        capture_output=True, text=True, timeout=8, check=False,
    )
    if result.returncode:
        return {"ok": False, "curl_exit": result.returncode}
    body, status, seconds, _ = result.stdout.rsplit("\n", 3)
    try:
        matches = ipaddress.ip_address(body.strip()) == ipaddress.ip_address(source)
    except ValueError:
        matches = False
    return {
        "ok": status == "200" and matches and len(body.encode()) <= 256,
        "status": int(status), "matches_source": matches,
        "total_ms": round(float(seconds) * 1000, 3),
    }


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", required=True, help="已绑定到本机的公网 IPv6；不会添加地址")
    parser.add_argument("--rounds", type=int, default=5)
    args = parser.parse_args()
    source = ipaddress.ip_address(args.source)
    if source.version != 6 or not source.is_global or not 3 <= args.rounds <= 20:
        parser.error("需要公网 IPv6，测速轮数为3至20")
    samples = {url: [] for url in URLS}
    # 轮换每轮的请求顺序，避免固定的先后位置影响比较；不复用HTTP连接。
    for round_index in range(args.rounds):
        start = round_index % len(URLS)
        for url in URLS[start:] + URLS[:start]:
            try:
                samples[url].append(sample(url, str(source)))
            except (subprocess.TimeoutExpired, ValueError):
                samples[url].append({"ok": False, "error": "timeout_or_invalid_response"})
    results = []
    for url, rows in samples.items():
        times = [row["total_ms"] for row in rows if row["ok"]]
        results.append({
            "url": url, "successes": len(times), "rounds": args.rounds,
            "median_ms": round(statistics.median(times), 3) if times else None,
            "samples": rows,
        })
    # 先比较成功次数，再比较包含DNS、TCP、TLS和响应读取的总耗时中位数。
    results.sort(key=lambda row: (-row["successes"], row["median_ms"] if row["median_ms"] is not None else float("inf")))
    print(json.dumps({
        "checked_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "source_ipv6": str(source), "method": "IPv6 HTTPS/1.1 fresh connections; success count then median total time",
        "probe_urls": [row["url"] for row in results], "results": results,
    }, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
