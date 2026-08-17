#![no_std]
extern crate alloc;

use alloc::collections::BTreeMap;
use alloc::format;
use alloc::string::String;
use alloc::string::ToString;
use alloc::vec::Vec;

use openagent_pdk::export::Plugin;
use openagent_pdk::host;
use openagent_pdk::prelude::*;
use openagent_pdk::types::{HttpResp, HttpRequest, Route};

struct SkillManager;

impl Plugin for SkillManager {
    fn plugin_type() -> &'static str {
        "cli:http"
    }
    fn name() -> &'static str {
        "skill-manager"
    }
    fn description() -> &'static str {
        "Installable skill management: list, detail, install, remove"
    }

    fn routes() -> Vec<Route> {
        vec![
            Route {
                method: "GET".into(),
                path: "/skills".into(),
                description: "List installable skills".into(),
            },
            Route {
                method: "GET".into(),
                path: "/skills/{name}".into(),
                description: "Get skill detail".into(),
            },
            Route {
                method: "POST".into(),
                path: "/skills/{name}/install".into(),
                description: "Install a skill".into(),
            },
            Route {
                method: "POST".into(),
                path: "/skills/{name}/remove".into(),
                description: "Remove a skill".into(),
            },
        ]
    }

    fn handle_http_request(req: &HttpRequest) -> HttpResp {
        // The host has already matched the route; branch on whether the
        // {name} param is present (detail/install/remove) or not (list).
        let name = req.params.get("name").map(|s| s.as_str()).unwrap_or("");
        match req.method.as_str() {
            "GET" => {
                if name.is_empty() {
                    handle_list_skills()
                } else if !valid_name(name) {
                    json_resp(400, r#"{"error":"invalid skill name"}"#)
                } else {
                    handle_skill_detail(name)
                }
            }
            "POST" => {
                if name.is_empty() {
                    return json_resp(400, r#"{"error":"missing skill name"}"#);
                }
                if !valid_name(name) {
                    return json_resp(400, r#"{"error":"invalid skill name"}"#);
                }
                if req.path.ends_with("/install") {
                    handle_install(name)
                } else if req.path.ends_with("/remove") {
                    handle_remove(name)
                } else {
                    json_resp(404, r#"{"error":"not found"}"#)
                }
            }
            _ => json_resp(405, r#"{"error":"method not allowed"}"#),
        }
    }
}

openagent_pdk::export!(SkillManager);

// ── helpers ──

fn json_resp(status: u16, body: &str) -> HttpResp {
    let mut headers = BTreeMap::new();
    headers.insert("content-type".into(), "application/json".into());
    HttpResp {
        status,
        headers,
        body: body.into(),
    }
}

fn json_resp_owned(status: u16, body: String) -> HttpResp {
    let mut headers = BTreeMap::new();
    headers.insert("content-type".into(), "application/json".into());
    HttpResp {
        status,
        headers,
        body,
    }
}

fn err_resp(status: u16, msg: &str) -> HttpResp {
    let body = format!(r#"{{"error":"{}"}}"#, json_escape(msg));
    json_resp_owned(status, body)
}

fn json_escape(s: &str) -> String {
    let mut out = String::new();
    for c in s.chars() {
        match c {
            '"' => out.push_str(r#"\""#),
            '\\' => out.push_str(r"\\"),
            '\n' => out.push_str(r"\n"),
            '\r' => out.push_str(r"\r"),
            '\t' => out.push_str(r"\t"),
            '\u{0008}' => out.push_str(r"\b"),
            '\u{000c}' => out.push_str(r"\f"),
            _ if (c as u32) < 0x20 => {
                out.push_str(&format!("\\u{:04x}", c as u32));
            }
            _ => out.push(c),
        }
    }
    out
}

/// Get the INSTALLABLE_SKILLS.json path from env or default.
fn json_path() -> Result<String, String> {
    match host::env_get("INSTALLABLE_SKILLS_PATH") {
        Ok(p) if !p.is_empty() => Ok(p),
        _ => {
            let home = host::env_get("HOME").map_err(|e| format!("env_get HOME: {}", e))?;
            Ok(format!("{}/.agents/INSTALLABLE_SKILLS.json", home))
        }
    }
}

/// Get the skills installation directory (~/.agents/skills).
fn skills_dir() -> Result<String, String> {
    let home = host::env_get("HOME").map_err(|e| format!("env_get HOME: {}", e))?;
    Ok(format!("{}/.agents/skills", home))
}

/// Build a minimal environment for install/remove subprocesses.
///
/// `host::exec_command` with `env=None` inherits the *entire* host
/// environment, leaking API keys and other secrets to the child process
/// tree (npm, postinstall scripts, transitive deps). We instead pass an
/// explicit allowlist: just enough for `npx skills add` to find the
/// registry, the home directory, and a working locale. Every other
/// variable — including `OPENAGENT_API_KEY` and any host-exported
/// credential — is dropped.
fn minimal_env() -> Result<alloc::collections::BTreeMap<String, String>, String> {
    let mut env = alloc::collections::BTreeMap::new();
    for key in &["HOME", "PATH", "LANG", "LC_ALL", "TERM"] {
        if let Ok(val) = host::env_get(key) {
            if !val.is_empty() {
                env.insert((*key).into(), val);
            }
        }
    }
    // npm/npx configuration — needed for registry access and proxy.
    for key in &["NPM_CONFIG_REGISTRY", "NPM_CONFIG_PROXY", "NPM_CONFIG_HTTPS_PROXY"] {
        if let Ok(val) = host::env_get(key) {
            if !val.is_empty() {
                env.insert((*key).into(), val);
            }
        }
    }
    Ok(env)
}

/// Validate that a catalog-supplied command is safe to execute.
///
/// The catalog is fetched from a remote git repository (refresh-skills.sh);
/// a MITM or repo compromise could inject an arbitrary `cmd`. We allow
/// only `npx` — the sole command in the shipped catalog — so an attacker
/// who corrupts the catalog still cannot run `/bin/sh` or `curl`.
fn safe_cmd(cmd: &str) -> bool {
    cmd == "npx"
}

/// Validate that catalog-supplied args do not pull from an untrusted URL.
///
/// `npx skills add <url>` fetches a git repository and runs its
/// postinstall scripts. A compromised catalog could point `url` at an
/// attacker-controlled host. We allow only `gitcode.com` — the sole
/// domain in the shipped catalog — so a corrupted catalog cannot widen
/// the supply chain to an arbitrary origin.
fn safe_args(args: &[&str]) -> bool {
    for a in args {
        if let Some(rest) = a
            .strip_prefix("https://")
            .or_else(|| a.strip_prefix("http://"))
            .or_else(|| a.strip_prefix("git://"))
        {
            // Strip any "user@host" or "host:port" prefix, take the host.
            let rest = rest.split('/').next().unwrap_or("");
            let host = rest.rsplit('@').next().unwrap_or("");
            // Strip port.
            let host = host.split(':').next().unwrap_or("");
            if host != "gitcode.com" {
                return false;
            }
        }
    }
    true
}

/// Read and parse the INSTALLABLE_SKILLS.json catalog.
fn load_catalog() -> Result<serde_json::Value, String> {
    let path = json_path()?;
    let bytes = host::fs_read(&path).map_err(|e| format!("read catalog: {}", e))?;
    let text =
        String::from_utf8(bytes).map_err(|e| format!("catalog not UTF-8: {}", e))?;
    serde_json::from_str(&text).map_err(|e| format!("parse catalog: {}", e))
}

/// Check if a skill is installed (SKILL.md exists on disk).
fn is_installed(skills_dir: &str, name: &str) -> bool {
    let path = format!("{}/{}/SKILL.md", skills_dir, name);
    host::fs_read(&path).is_ok()
}

/// Compute upgradeable: installed && localMD5 != remoteMD5.
fn is_upgradeable(skills_dir: &str, name: &str, remote_md5: &str) -> bool {
    if remote_md5.is_empty() {
        return false;
    }
    let dir = format!("{}/{}", skills_dir, name);
    match host::directory_md5(&dir) {
        Ok(local) => local != remote_md5,
        Err(_) => false,
    }
}

/// Reject path-traversal names: empty, ".", "..", or anything containing
/// a path separator. We check both `/` and `\` so the guard holds on
/// POSIX and Windows — a backslash is a legal filename char on Linux
/// (no traversal) but a separator on Windows.
fn valid_name(name: &str) -> bool {
    !name.is_empty()
        && name != "."
        && name != ".."
        && !name.contains('/')
        && !name.contains('\\')
}

/// Extract description from a frontmatter map.
fn frontmatter_desc(fm: &serde_json::Value) -> String {
    fm.get("description")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string()
}

// ── handlers ──

fn handle_list_skills() -> HttpResp {
    let catalog = match load_catalog() {
        Ok(c) => c,
        Err(e) => return err_resp(500, &e),
    };

    let skills_arr = match catalog.get("skills").and_then(|v| v.as_array()) {
        Some(a) => a,
        None => return err_resp(500, "catalog missing skills array"),
    };

    let sdir = match skills_dir() {
        Ok(d) => d,
        Err(e) => return err_resp(500, &e),
    };

    let mut items: Vec<serde_json::Value> = Vec::new();
    for entry in skills_arr {
        let name = entry
            .get("name")
            .and_then(|v| v.as_str())
            .unwrap_or("");
        if name.is_empty() {
            continue;
        }
        // The catalog is trusted, but a corrupted/tampered catalog could
        // carry a traversal name ("../foo"). Skip any entry whose name
        // would escape the skills directory — same guard the detail/
        // install/remove handlers apply to the URL parameter.
        if !valid_name(name) {
            continue;
        }

        let fm = entry.get("frontmatter").cloned().unwrap_or_default();
        let desc = frontmatter_desc(&fm);
        let installed = is_installed(&sdir, name);
        let remote_md5 = entry
            .get("skill_folder_md5")
            .and_then(|v| v.as_str())
            .unwrap_or("");
        let upgradeable = if installed {
            is_upgradeable(&sdir, name, remote_md5)
        } else {
            false
        };

        items.push(serde_json::json!({
            "name": name,
            "description": desc,
            "frontmatter": fm,
            "installed": installed,
            "upgradeable": upgradeable,
            "install_cmd": entry.get("install_cmd").cloned().unwrap_or_default(),
            "remove_cmd": entry.get("remove_cmd").cloned().unwrap_or_default(),
        }));
    }

    let body = serde_json::json!({ "skills": items });
    match serde_json::to_string(&body) {
        Ok(s) => json_resp_owned(200, s),
        Err(e) => err_resp(500, &format!("serialize: {}", e)),
    }
}

fn handle_skill_detail(name: &str) -> HttpResp {
    let catalog = match load_catalog() {
        Ok(c) => c,
        Err(e) => return err_resp(500, &e),
    };

    let skills_arr = match catalog.get("skills").and_then(|v| v.as_array()) {
        Some(a) => a,
        None => return err_resp(500, "catalog missing skills array"),
    };

    let entry = skills_arr
        .iter()
        .find(|e| e.get("name").and_then(|v| v.as_str()) == Some(name));
    let entry = match entry {
        Some(e) => e,
        None => return err_resp(404, "skill not found"),
    };

    let sdir = match skills_dir() {
        Ok(d) => d,
        Err(e) => return err_resp(500, &e),
    };

    let fm = entry.get("frontmatter").cloned().unwrap_or_default();
    let desc = frontmatter_desc(&fm);
    let installed = is_installed(&sdir, name);
    let remote_md5 = entry
        .get("skill_folder_md5")
        .and_then(|v| v.as_str())
        .unwrap_or("");
    let upgradeable = if installed {
        is_upgradeable(&sdir, name, remote_md5)
    } else {
        false
    };

    let body = serde_json::json!({
        "name": name,
        "description": desc,
        "frontmatter": fm,
        "skill_md": entry.get("skill_md").and_then(|v| v.as_str()).unwrap_or(""),
        "installed": installed,
        "upgradeable": upgradeable,
        "install_cmd": entry.get("install_cmd").cloned().unwrap_or_default(),
        "remove_cmd": entry.get("remove_cmd").cloned().unwrap_or_default(),
    });

    match serde_json::to_string(&body) {
        Ok(s) => json_resp_owned(200, s),
        Err(e) => err_resp(500, &format!("serialize: {}", e)),
    }
}

fn handle_install(name: &str) -> HttpResp {
    let catalog = match load_catalog() {
        Ok(c) => c,
        Err(e) => return err_resp(500, &e),
    };

    let skills_arr = match catalog.get("skills").and_then(|v| v.as_array()) {
        Some(a) => a,
        None => return err_resp(500, "catalog missing skills array"),
    };

    let entry = skills_arr
        .iter()
        .find(|e| e.get("name").and_then(|v| v.as_str()) == Some(name));
    let entry = match entry {
        Some(e) => e,
        None => return err_resp(404, "skill not found"),
    };

    // Extract install_cmd {cmd, args[]}
    let cmd = entry
        .get("install_cmd")
        .and_then(|c| c.get("cmd"))
        .and_then(|v| v.as_str())
        .unwrap_or("");
    if cmd.is_empty() {
        return err_resp(500, "install_cmd.cmd is empty");
    }
    // Allowlist: the catalog is fetched from a remote repo; a compromised
    // catalog could inject an arbitrary program. Only `npx` is permitted.
    if !safe_cmd(cmd) {
        return err_resp(500, "install_cmd.cmd not in allowlist");
    }

    let args: Vec<String> = entry
        .get("install_cmd")
        .and_then(|c| c.get("args"))
        .and_then(|v| v.as_array())
        .map(|a| {
            a.iter()
                .map(|v| v.as_str().unwrap_or("").to_string())
                .collect()
        })
        .unwrap_or_default();
    let args_ref: Vec<&str> = args.iter().map(|s| s.as_str()).collect();

    // URL domain allowlist: a compromised catalog could point the install
    // URL at an attacker-controlled host. Only gitcode.com is permitted.
    if !safe_args(&args_ref) {
        return err_resp(500, "install_cmd.args contains untrusted URL");
    }

    // Minimal env with replace semantics: the child sees ONLY these
    // variables — host secrets (API keys, tokens) are excluded entirely.
    let env = match minimal_env() {
        Ok(e) => e,
        Err(e) => return err_resp(500, &e),
    };

    // Execute install command (env_replace=true → no host env inheritance)
    let result = match host::exec_command(cmd, &args_ref, None, Some(&env), true, Some(120_000)) {
        Ok(r) => r,
        Err(e) => return err_resp(500, &format!("exec: {}", e)),
    };

    // exec_command error (command could not run / timed out)
    if !result.error.is_empty() {
        return err_resp(500, &result.error);
    }
    // Non-zero exit code = business failure
    if result.exit_code != 0 {
        let body = format!(
            r#"{{"error":"install failed (exit {})","stderr":"{}"}}"#,
            result.exit_code,
            json_escape(&result.stderr)
        );
        return json_resp_owned(500, body);
    }

    // Verify SKILL.md appeared on disk
    let sdir = match skills_dir() {
        Ok(d) => d,
        Err(e) => return err_resp(500, &e),
    };
    if !is_installed(&sdir, name) {
        let body = format!(
            r#"{{"error":"install command succeeded but SKILL.md not found","stderr":"{}"}}"#,
            json_escape(&result.stderr)
        );
        return json_resp_owned(500, body);
    }

    // Success — check upgradeable (should be false right after install)
    let remote_md5 = entry
        .get("skill_folder_md5")
        .and_then(|v| v.as_str())
        .unwrap_or("");
    let upgradeable = is_upgradeable(&sdir, name, remote_md5);

    let body = format!(
        r#"{{"installed":true,"upgradeable":{}}}"#,
        upgradeable
    );
    json_resp_owned(200, body)
}

fn handle_remove(name: &str) -> HttpResp {
    let catalog = match load_catalog() {
        Ok(c) => c,
        Err(e) => return err_resp(500, &e),
    };

    let skills_arr = match catalog.get("skills").and_then(|v| v.as_array()) {
        Some(a) => a,
        None => return err_resp(500, "catalog missing skills array"),
    };

    let entry = skills_arr
        .iter()
        .find(|e| e.get("name").and_then(|v| v.as_str()) == Some(name));
    let entry = match entry {
        Some(e) => e,
        None => return err_resp(404, "skill not found"),
    };

    // Extract remove_cmd {cmd, args[]}
    let cmd = entry
        .get("remove_cmd")
        .and_then(|c| c.get("cmd"))
        .and_then(|v| v.as_str())
        .unwrap_or("");
    if cmd.is_empty() {
        return err_resp(500, "remove_cmd.cmd is empty");
    }
    if !safe_cmd(cmd) {
        return err_resp(500, "remove_cmd.cmd not in allowlist");
    }

    let args: Vec<String> = entry
        .get("remove_cmd")
        .and_then(|c| c.get("args"))
        .and_then(|v| v.as_array())
        .map(|a| {
            a.iter()
                .map(|v| v.as_str().unwrap_or("").to_string())
                .collect()
        })
        .unwrap_or_default();
    let args_ref: Vec<&str> = args.iter().map(|s| s.as_str()).collect();

    if !safe_args(&args_ref) {
        return err_resp(500, "remove_cmd.args contains untrusted URL");
    }

    let env = match minimal_env() {
        Ok(e) => e,
        Err(e) => return err_resp(500, &e),
    };

    // Execute remove command (env_replace=true → no host env inheritance)
    let result = match host::exec_command(cmd, &args_ref, None, Some(&env), true, Some(120_000)) {
        Ok(r) => r,
        Err(e) => return err_resp(500, &format!("exec: {}", e)),
    };

    if !result.error.is_empty() {
        return err_resp(500, &result.error);
    }
    if result.exit_code != 0 {
        let body = format!(
            r#"{{"error":"remove failed (exit {})","stderr":"{}"}}"#,
            result.exit_code,
            json_escape(&result.stderr)
        );
        return json_resp_owned(500, body);
    }

    // Verify SKILL.md is gone
    let sdir = match skills_dir() {
        Ok(d) => d,
        Err(e) => return err_resp(500, &e),
    };
    if is_installed(&sdir, name) {
        return json_resp_owned(
            500,
            r#"{"error":"remove command succeeded but SKILL.md still exists"}"#
                .to_string(),
        );
    }

    json_resp(200, r#"{"installed":false}"#)
}
