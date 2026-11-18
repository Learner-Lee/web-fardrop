import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time

release = Path("/storage/blog/web-fardrop")
backups = release / "backups"
backups.mkdir(mode=0o700, exist_ok=True)
nginx_site = Path("/etc/nginx/sites-available/blog")
nginx_snippet = Path("/etc/nginx/snippets/web-fardrop.conf")
about = Path("/storage/blog/2025-blog-public/data/config/about.json")
stamp = str(time.time_ns())

def write_atomic(path, text):
    metadata = path.stat()
    with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=path.parent, delete=False) as temp:
        temp.write(text)
        temp.flush()
        os.fsync(temp.fileno())
        os.fchmod(temp.fileno(), metadata.st_mode & 0o777)
        os.fchown(temp.fileno(), metadata.st_uid, metadata.st_gid)
    os.replace(temp.name, path)

def run(*args):
    subprocess.run(args, check=True)

entry = json.loads((release / "about-entry.json").read_text())
data = json.loads(about.read_text())
works = data["works"]
existing = next((work for work in works if work["id"] == entry["id"]), None)
if existing and existing != entry:
    raise SystemExit("About already contains an independently edited FarDrop entry")

site_text = nginx_site.read_text()
anchor = "    # AI-Sub knowledge reader\n"
line = "    include /etc/nginx/snippets/web-fardrop.conf;\n"
if line not in site_text and site_text.count(anchor) != 1:
    raise SystemExit("Cannot identify the blog HTTPS server block")

if nginx_snippet.exists():
    shutil.copy2(nginx_snippet, backups / f"nginx-snippet-{stamp}")
run("install", "-m", "0644", str(release / "nginx.conf"), str(nginx_snippet))

if line not in site_text:
    shutil.copy2(nginx_site, backups / f"nginx-blog-{stamp}")
    write_atomic(nginx_site, site_text.replace(anchor, line + "\n" + anchor, 1))

try:
    run("nginx", "-t")
    run("systemctl", "reload", "nginx")
    run("curl", "-fkSs", "--max-time", "10", "--resolve",
        "web.alenli.cn:443:127.0.0.1", "-o", "/dev/null",
        "https://web.alenli.cn/about/fardrop/")
except subprocess.CalledProcessError:
    write_atomic(nginx_site, site_text)
    previous_snippet = backups / f"nginx-snippet-{stamp}"
    if previous_snippet.exists():
        write_atomic(nginx_snippet, previous_snippet.read_text())
    run("nginx", "-t")
    run("systemctl", "reload", "nginx")
    raise

if not existing:
    shutil.copy2(about, backups / f"about-{stamp}.json")
    pos = next((i for i, work in enumerate(works) if work["id"] == "quickdrop"), 0)
    works.insert(pos, entry)
    write_atomic(about, json.dumps(data, ensure_ascii=False, indent=2) + "\n")
print("FarDrop route and About entry published")
