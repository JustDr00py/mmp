#!/usr/bin/env python3
"""Rebuild gluetun's NordVPN server data from Nord's live API.

gluetun's bundled NordVPN list can predate a rotation of Nord's WireGuard
server keys. Servers then silently drop handshakes built with the retired
keys: gluetun logs "Wireguard setup is complete" but never passes its
healthcheck. Its own updater cannot repair this while the tunnel is down,
because the container's DNS and HTTP go through the tunnel.

This script replaces only the NordVPN section of the shared gluetun volume's
servers.json (marking the old file as servers.json.bak) and leaves every
other provider untouched. gluetun needs the section at the current schema
version and with a recent timestamp, or it discards it and falls back to its
bundled data. Restart the stack afterwards; mmp must be recreated too, since
it lives in gluetun's network namespace:

    docker compose -f docker-compose.vpn.yml up -d --force-recreate gluetun mmp

Usage:
    scripts/refresh-nordvpn-servers.py
    scripts/refresh-nordvpn-servers.py --volume myproj_gluetun --runtime podman
    scripts/refresh-nordvpn-servers.py --dry-run   # keep the result on the host
"""

import argparse
import json
import os
import subprocess
import sys
import tempfile
import time
import urllib.request
import uuid

API = "https://api.nordvpn.com/v1/servers?limit=0"
# Schema version gluetun v3 understands. Its bundled data uses it, and a
# section with an older version is ignored.
SCHEMA_VERSION = 5
CONTINENTS = {"Europe", "North America", "South America", "Asia", "Africa", "Oceania"}
# A live list this small is more likely a broken API response than reality.
MIN_WIREGUARD = 100
MIN_OPENVPN = 100


def run(cmd, quiet=False):
    return subprocess.run(cmd, check=not quiet, capture_output=quiet, text=True)


def ip_string(value):
    while isinstance(value, dict):
        value = value.get("ip")
    return value if isinstance(value, str) else None


def fetch_live():
    request = urllib.request.Request(API, headers={"User-Agent": "mmp-refresh/1"})
    with urllib.request.urlopen(request, timeout=180) as response:
        return json.load(response)


def build_section(live):
    servers = []
    for server in live:
        hostname = server.get("hostname")
        if not hostname:
            continue
        location = (server.get("locations") or [{}])[0]
        country = location.get("country") or {}
        ips = [ip for ip in (ip_string(i) for i in server.get("ips") or []) if ip]
        if not ips and server.get("station"):
            ips = [server["station"]]
        if not ips:
            continue
        titles = [g.get("title", "") for g in server.get("groups", [])]
        region = next((t for t in titles if t in CONTINENTS), "")
        try:
            number = int(server.get("name", "").rsplit("#", 1)[1])
        except (IndexError, ValueError):
            number = server.get("id")
        base = {
            "country": country.get("name", ""),
            "region": region,
            "city": (country.get("city") or {}).get("name", ""),
            "categories": [t for t in titles if t not in CONTINENTS],
            "number": number,
            "hostname": hostname,
            "ips": ips,
        }
        technologies = {t["identifier"]: t for t in server.get("technologies", [])}

        wireguard = technologies.get("wireguard_udp")
        if wireguard:
            public_key = next(
                (m["value"] for m in wireguard.get("metadata", []) if m["name"] == "public_key"),
                None,
            )
            if public_key:
                servers.append({"vpn": "wireguard", **base, "wgpubkey": public_key})

        if "openvpn_udp" in technologies or "openvpn_tcp" in technologies:
            servers.append({
                "vpn": "openvpn",
                **base,
                "tcp": "openvpn_tcp" in technologies,
                "udp": "openvpn_udp" in technologies,
            })

    wireless = sum(1 for s in servers if s["vpn"] == "wireguard")
    openvpn = len(servers) - wireless
    return {"version": SCHEMA_VERSION, "timestamp": int(time.time()), "servers": servers}, wireless, openvpn


def read_existing(runtime, volume, image):
    name = f"mmp-refresh-{uuid.uuid4().hex[:8]}"
    created = run([runtime, "create", "--name", name, "-v", f"{volume}:/gluetun", image], quiet=True)
    if created.returncode != 0:
        sys.exit(f"cannot create a helper container: {created.stderr.strip()}")
    temp = tempfile.NamedTemporaryFile(prefix="servers.", suffix=".json", delete=False)
    temp.close()
    copied = run([runtime, "cp", f"{name}:/gluetun/servers.json", temp.name], quiet=True)
    if copied.returncode != 0:
        os.unlink(temp.name)
        return name, None, None
    with open(temp.name) as f:
        data = json.load(f)
    return name, data, temp.name


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--volume", default="mmp_gluetun", help="gluetun volume (default: mmp_gluetun)")
    parser.add_argument("--runtime", default="docker", help="container runtime (default: docker)")
    parser.add_argument("--image", default="docker.io/qmcgaw/gluetun:v3", help="image for the helper container (docker cp only)")
    parser.add_argument("--dry-run", action="store_true", help="write the result to a temp file and do not touch the volume")
    parser.add_argument("--force", action="store_true", help="accept an unusually small live list")
    args = parser.parse_args()

    print(f"fetching {API}")
    live = fetch_live()
    print(f"live servers: {len(live)}")
    section, wireless, openvpn = build_section(live)
    print(f"built NordVPN entries: {wireless} WireGuard, {openvpn} OpenVPN")
    if not args.force and (wireless < MIN_WIREGUARD or openvpn < MIN_OPENVPN):
        sys.exit("refusing to write an unusually small list; rerun with --force if that is expected")

    name, data, old_path = read_existing(args.runtime, args.volume, args.image)
    had_file = data is not None
    try:
        if data is None:
            data = {"version": 1}
            print("no servers.json in the volume; writing a new one")
        data["nordvpn"] = section
        out = tempfile.NamedTemporaryFile(
            prefix="servers.new.", suffix=".json", delete=False, mode="w")
        with out:
            json.dump(data, out, separators=(",", ":"))
        os.chmod(out.name, 0o644)
        if os.path.getsize(out.name) == 0:
            sys.exit("refusing to write an empty file")
        if args.dry_run:
            print(f"dry run: wrote {out.name}; volume untouched")
            return
        # Keep the previous file next to it for a manual rollback.
        if had_file:
            run([args.runtime, "cp", old_path, f"{name}:/gluetun/servers.json.bak"], quiet=True)
        run([args.runtime, "cp", out.name, f"{name}:/gluetun/servers.json"])
        print("updated servers.json in volume", args.volume)
        print("now recreate both containers (mmp shares gluetun's network):")
        print("  docker compose -f docker-compose.vpn.yml up -d --force-recreate gluetun mmp")
    finally:
        run([args.runtime, "rm", "-f", name], quiet=True)
        if old_path:
            os.unlink(old_path)


if __name__ == "__main__":
    main()
