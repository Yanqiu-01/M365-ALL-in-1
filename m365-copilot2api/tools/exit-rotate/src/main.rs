//! 换出口 IP 的独立 CLI。Go 网关在每次注册前优先调用本程序；找不到可执行
//! 文件时回退到 internal/exitrotate 的纯 Go 实现。
//!
//! 输入输出都是 JSON，方便 APK / Windows / RikkaHub 共用同一份契约：
//!
//! ```text
//! echo '{"mode":"clash","clashApi":"http://127.0.0.1:9097","clashGroup":"PROXY","clashNode":"jp-01"}' \
//!   | exit-rotate --json
//! ```

use std::env;
use std::io::{self, Read, Write};
use std::process::{Command, Stdio};
use std::thread;
use std::time::Duration;

fn main() {
    let json_mode = env::args().any(|a| a == "--json" || a == "-j");
    let mut raw = String::new();
    if let Err(err) = io::stdin().read_to_string(&mut raw) {
        fail(&format!("read stdin: {err}"), json_mode);
    }
    let req = match parse_request(&raw) {
        Ok(v) => v,
        Err(err) => fail(&err, json_mode),
    };
    match rotate(&req) {
        Ok(result) => emit(&result, json_mode),
        Err(err) => {
            emit(
                &Result {
                    ok: false,
                    mode: req.mode.clone(),
                    ip: String::new(),
                    prev_ip: req.prev_ip.clone(),
                    changed: false,
                    detail: err,
                },
                json_mode,
            );
            std::process::exit(1);
        }
    }
}

#[derive(Clone, Default)]
struct Request {
    mode: String,
    prev_ip: String,
    phone_socks: String,
    adb: String,
    clash_api: String,
    clash_secret: String,
    clash_group: String,
    clash_proxy: String,
    clash_node: String,
    expect_ip: String,
    probe_proxy: String,
}

struct Result {
    ok: bool,
    mode: String,
    ip: String,
    prev_ip: String,
    changed: bool,
    detail: String,
}

fn parse_request(raw: &str) -> std::result::Result<Request, String> {
    let mut req = Request::default();
    req.mode = json_string(raw, "mode").unwrap_or_default();
    req.prev_ip = json_string(raw, "prevIp").unwrap_or_default();
    req.phone_socks = json_string(raw, "phoneSocks").unwrap_or_default();
    req.adb = json_string(raw, "adb").unwrap_or_default();
    req.clash_api = json_string(raw, "clashApi").unwrap_or_default();
    req.clash_secret = json_string(raw, "clashSecret").unwrap_or_default();
    req.clash_group = json_string(raw, "clashGroup").unwrap_or_default();
    req.clash_proxy = json_string(raw, "clashProxy").unwrap_or_default();
    req.clash_node = json_string(raw, "clashNode").unwrap_or_default();
    req.expect_ip = json_string(raw, "expectIp").unwrap_or_default();
    req.probe_proxy = json_string(raw, "probeProxy").unwrap_or_default();
    if req.mode.trim().is_empty() {
        return Err("mode is required".into());
    }
    Ok(req)
}

fn json_string(raw: &str, key: &str) -> Option<String> {
    let needle = format!("\"{key}\"");
    let rest = raw.split(&needle).nth(1)?;
    let rest = rest.trim_start();
    let rest = rest.strip_prefix(':')?.trim_start();
    if rest.starts_with("null") {
        return None;
    }
    if !rest.starts_with('"') {
        return None;
    }
    let mut out = String::new();
    let mut chars = rest[1..].chars();
    while let Some(c) = chars.next() {
        match c {
            '"' => break,
            '\\' => {
                if let Some(n) = chars.next() {
                    out.push(n);
                }
            }
            other => out.push(other),
        }
    }
    Some(out)
}

fn rotate(req: &Request) -> std::result::Result<Result, String> {
    match req.mode.to_ascii_lowercase().as_str() {
        "phone" => rotate_phone(req),
        "clash" => rotate_clash(req),
        "proxy" | "ip" => {
            let proxy = first_non_empty(&[&req.probe_proxy, &req.phone_socks, &req.clash_proxy]);
            let ip = probe_ip(&proxy)?;
            Ok(ok_result(req, ip, "probed current exit"))
        }
        other => Err(format!("unsupported exit-rotate mode: {other}")),
    }
}

fn rotate_phone(req: &Request) -> std::result::Result<Result, String> {
    let socks = if req.phone_socks.trim().is_empty() {
        "socks5://127.0.0.1:1081"
    } else {
        req.phone_socks.trim()
    };
    let mut prev = req.prev_ip.clone();
    if let Ok(ip) = probe_ip(socks) {
        if !prev.is_empty() && ip != prev {
            return Ok(ok_result(req, ip, "current exit already differs"));
        }
        if prev.is_empty() {
            prev = ip;
        }
    }
    let mut last = String::from("exit IP unchanged");
    for _ in 0..6 {
        toggle_airplane(&req.adb)?;
        match probe_ip(socks) {
            Ok(ip) if !ip.is_empty() && ip != prev => {
                return Ok(ok_result(req, ip, "rotated via airplane-mode"));
            }
            Ok(ip) => last = format!("exit IP unchanged ({ip})"),
            Err(err) => last = err,
        }
    }
    Err(last)
}

fn toggle_airplane(adb: &str) -> std::result::Result<(), String> {
    if cfg!(target_os = "android") {
        if run(&["cmd", "connectivity", "airplane-mode", "enable"]).is_ok() {
            thread::sleep(Duration::from_secs(3));
            return run(&["cmd", "connectivity", "airplane-mode", "disable"]);
        }
    }
    let bin = if adb.trim().is_empty() { "adb" } else { adb };
    run(&[bin, "shell", "cmd", "connectivity", "airplane-mode", "enable"])?;
    thread::sleep(Duration::from_secs(3));
    run(&[bin, "shell", "cmd", "connectivity", "airplane-mode", "disable"])
}

fn rotate_clash(req: &Request) -> std::result::Result<Result, String> {
    if req.clash_api.trim().is_empty() || req.clash_group.trim().is_empty() || req.clash_node.trim().is_empty()
    {
        return Err("clash api, group and node are required".into());
    }
    let url = format!(
        "{}/proxies/{}",
        req.clash_api.trim_end_matches('/'),
        req.clash_group
    );
    let body = format!("{{\"name\":\"{}\"}}", json_escape(&req.clash_node));
    http_put(&url, &req.clash_secret, &body)?;
    thread::sleep(Duration::from_secs(3));
    let proxy = first_non_empty(&[&req.clash_proxy, &req.probe_proxy]);
    let ip = probe_ip(&proxy)?;
    let mut detail = String::from("switched clash node");
    if !req.expect_ip.is_empty() && req.expect_ip != ip {
        detail = format!("exit IP {ip} differs from expected {}", req.expect_ip);
    }
    Ok(ok_result(req, ip, &detail))
}

fn probe_ip(proxy: &str) -> std::result::Result<String, String> {
    let endpoints = [
        "https://api.ipify.org",
        "https://icanhazip.com",
        "https://ifconfig.me/ip",
    ];
    let mut last = String::from("exit IP probe failed");
    for endpoint in endpoints {
        match http_get(endpoint, proxy) {
            Ok(body) => {
                if let Some(ip) = clean_ipv4(&body) {
                    return Ok(ip);
                }
                last = format!("no IPv4 in {endpoint} response");
            }
            Err(err) => last = err,
        }
    }
    Err(last)
}

fn http_get(url: &str, _proxy: &str) -> std::result::Result<String, String> {
    // 探测走系统 curl：本 CLI 刻意不引入 reqwest，保持零依赖、可在 Android
    // 交叉编译。Go 回退路径使用 outbound.New，能走 SOCKS5 / HTTP 代理。
    let mut cmd = Command::new("curl");
    cmd.args(["-fsS", "--max-time", "8", url])
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    let out = cmd.output().map_err(|e| format!("curl: {e}"))?;
    if !out.status.success() {
        return Err(format!(
            "curl {}: {}",
            url,
            String::from_utf8_lossy(&out.stderr).trim()
        ));
    }
    Ok(String::from_utf8_lossy(&out.stdout).to_string())
}

fn http_put(url: &str, secret: &str, body: &str) -> std::result::Result<(), String> {
    let mut cmd = Command::new("curl");
    cmd.args(["-fsS", "-X", "PUT", "--max-time", "10", "-H", "Content-Type: application/json"]);
    if !secret.trim().is_empty() {
        cmd.args(["-H", &format!("Authorization: Bearer {secret}")]);
    }
    cmd.args(["--data", body, url]);
    let out = cmd.output().map_err(|e| format!("curl: {e}"))?;
    if !out.status.success() {
        return Err(format!(
            "clash switch failed: {}",
            String::from_utf8_lossy(&out.stderr).trim()
        ));
    }
    Ok(())
}

fn run(args: &[&str]) -> std::result::Result<(), String> {
    if args.is_empty() {
        return Err("empty command".into());
    }
    let out = Command::new(args[0])
        .args(&args[1..])
        .output()
        .map_err(|e| format!("{}: {e}", args[0]))?;
    if !out.status.success() {
        return Err(format!(
            "{}: {}",
            args[0],
            String::from_utf8_lossy(&out.stderr).trim()
        ));
    }
    Ok(())
}

fn clean_ipv4(text: &str) -> Option<String> {
    let token = text.split_whitespace().next()?.trim();
    let mut parts = token.split('.');
    for _ in 0..4 {
        let part = parts.next()?;
        let n: u8 = part.parse().ok()?;
        let _ = n;
    }
    if parts.next().is_some() {
        return None;
    }
    Some(token.to_string())
}

fn ok_result(req: &Request, ip: String, detail: &str) -> Result {
    Result {
        ok: true,
        mode: req.mode.clone(),
        changed: !ip.is_empty() && ip != req.prev_ip,
        prev_ip: req.prev_ip.clone(),
        ip,
        detail: detail.to_string(),
    }
}

fn emit(result: &Result, json_mode: bool) {
    if json_mode {
        let payload = format!(
            "{{\"ok\":{},\"mode\":\"{}\",\"ip\":\"{}\",\"prevIp\":\"{}\",\"changed\":{},\"detail\":\"{}\"}}\n",
            result.ok,
            json_escape(&result.mode),
            json_escape(&result.ip),
            json_escape(&result.prev_ip),
            result.changed,
            json_escape(&result.detail)
        );
        let _ = io::stdout().write_all(payload.as_bytes());
        return;
    }
    println!(
        "ok={} mode={} ip={} changed={} {}",
        result.ok, result.mode, result.ip, result.changed, result.detail
    );
}

fn fail(msg: &str, json_mode: bool) -> ! {
    emit(
        &Result {
            ok: false,
            mode: String::new(),
            ip: String::new(),
            prev_ip: String::new(),
            changed: false,
            detail: msg.to_string(),
        },
        json_mode,
    );
    std::process::exit(1);
}

fn json_escape(s: &str) -> String {
    s.replace('\\', "\\\\").replace('"', "\\\"")
}

fn first_non_empty(values: &[&str]) -> String {
    values
        .iter()
        .map(|v| v.trim())
        .find(|v| !v.is_empty())
        .unwrap_or("")
        .to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_mode_and_node() {
        let req = parse_request(r#"{"mode":"clash","clashNode":"jp-01","prevIp":"1.1.1.1"}"#).unwrap();
        assert_eq!(req.mode, "clash");
        assert_eq!(req.clash_node, "jp-01");
        assert_eq!(req.prev_ip, "1.1.1.1");
    }

    #[test]
    fn cleans_ipv4() {
        assert_eq!(clean_ipv4(" 8.8.8.8\n").as_deref(), Some("8.8.8.8"));
        assert_eq!(clean_ipv4("<html>429</html>"), None);
    }

    #[test]
    fn rejects_unknown_mode() {
        let req = Request {
            mode: "browser".into(),
            ..Request::default()
        };
        assert!(rotate(&req).is_err());
    }
}
