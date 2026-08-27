// DevMan desktop shell.
//
// The window is a thin host: every piece of DevMan state comes from the daemon's
// HTTP API, exactly as it does for the CLI. The only things Rust does here are
// the two the webview cannot: find the daemon's discovery file and token on
// disk, and start the daemon when it is not running.

use std::fs;
use std::net::{SocketAddr, TcpStream, ToSocketAddrs};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::{Duration, Instant};

use serde::Serialize;

/// Endpoint is what the frontend needs to talk to the daemon.
///
/// The token is handed to the webview because the webview is the client; it is
/// never written anywhere, and the CSP restricts where the page may send it.
#[derive(Serialize, Clone)]
pub struct Endpoint {
    pub base_url: String,
    pub token: String,
    pub host: String,
    pub port: u16,
    pub pid: i64,
    pub version: String,
    pub api_version: String,
}

/// Layout mirrors the Go side's paths.Layout, so the About/Environment pages can
/// show where DevMan keeps its state even when the daemon is down.
#[derive(Serialize, Clone)]
pub struct Layout {
    pub home: String,
    pub settings: String,
    pub database: String,
    pub daemon: String,
    pub auth_token: String,
    pub logs: String,
}

fn home_dir() -> Option<PathBuf> {
    #[cfg(windows)]
    {
        std::env::var_os("USERPROFILE").map(PathBuf::from)
    }
    #[cfg(not(windows))]
    {
        std::env::var_os("HOME").map(PathBuf::from)
    }
}

/// data_home resolves the DevMan data directory with the same rules as the Go
/// implementation. Duplicating the rule is deliberate: the alternative is asking
/// a daemon that may not be running where its own files are.
fn data_home() -> Result<PathBuf, String> {
    if let Some(override_dir) = std::env::var_os("DEVMAN_HOME") {
        let path = PathBuf::from(override_dir);
        if !path.as_os_str().is_empty() {
            return Ok(path);
        }
    }

    #[cfg(windows)]
    {
        if let Some(base) = std::env::var_os("LOCALAPPDATA") {
            return Ok(PathBuf::from(base).join("DevMan"));
        }
        let home = home_dir().ok_or_else(|| "cannot resolve the user profile".to_string())?;
        return Ok(home.join("AppData").join("Local").join("DevMan"));
    }

    #[cfg(target_os = "macos")]
    {
        let home = home_dir().ok_or_else(|| "cannot resolve the home directory".to_string())?;
        return Ok(home
            .join("Library")
            .join("Application Support")
            .join("DevMan"));
    }

    #[cfg(all(not(windows), not(target_os = "macos")))]
    {
        if let Some(base) = std::env::var_os("XDG_STATE_HOME") {
            return Ok(PathBuf::from(base).join("devman"));
        }
        let home = home_dir().ok_or_else(|| "cannot resolve the home directory".to_string())?;
        Ok(home.join(".local").join("state").join("devman"))
    }
}

fn layout_for(home: &Path) -> Layout {
    Layout {
        home: home.display().to_string(),
        settings: home.join("config.yaml").display().to_string(),
        database: home.join("devman.db").display().to_string(),
        daemon: home.join("daemon.json").display().to_string(),
        auth_token: home.join("auth-token").display().to_string(),
        logs: home.join("logs").display().to_string(),
    }
}

#[tauri::command]
fn devman_paths() -> Result<Layout, String> {
    Ok(layout_for(&data_home()?))
}

/// listening reports whether something answers on the recorded address.
///
/// A recorded PID is not enough on its own: PIDs are reused, and a daemon that
/// died leaves its discovery file behind. Connecting is the cheap, honest check.
fn listening(host: &str, port: u16) -> bool {
    let target = format!("{host}:{port}");
    let addresses: Vec<SocketAddr> = match target.to_socket_addrs() {
        Ok(iter) => iter.collect(),
        Err(_) => return false,
    };
    addresses
        .iter()
        .any(|address| TcpStream::connect_timeout(address, Duration::from_millis(400)).is_ok())
}

fn read_endpoint() -> Result<Endpoint, String> {
    let home = data_home()?;
    let layout = layout_for(&home);

    let discovery = fs::read_to_string(&layout.daemon)
        .map_err(|_| "the DevMan daemon is not running".to_string())?;
    let record: serde_json::Value = serde_json::from_str(&discovery)
        .map_err(|_| "the daemon discovery file is unreadable".to_string())?;

    let host = record
        .get("host")
        .and_then(|value| value.as_str())
        .unwrap_or("127.0.0.1")
        .to_string();
    let port = record
        .get("port")
        .and_then(|value| value.as_u64())
        .ok_or_else(|| "the daemon discovery file has no port".to_string())? as u16;

    if !listening(&host, port) {
        return Err("the DevMan daemon is not running".to_string());
    }

    let token = fs::read_to_string(&layout.auth_token)
        .map_err(|_| "cannot read the DevMan auth token".to_string())?
        .trim()
        .to_string();
    if token.is_empty() {
        return Err("the DevMan auth token file is empty".to_string());
    }

    let api_version = record
        .get("api_version")
        .and_then(|value| value.as_str())
        .unwrap_or("v1")
        .to_string();

    Ok(Endpoint {
        base_url: format!("http://{host}:{port}/api/{api_version}"),
        token,
        host,
        port,
        pid: record.get("pid").and_then(|value| value.as_i64()).unwrap_or(0),
        version: record
            .get("version")
            .and_then(|value| value.as_str())
            .unwrap_or("")
            .to_string(),
        api_version,
    })
}

#[tauri::command]
fn daemon_endpoint() -> Result<Endpoint, String> {
    read_endpoint()
}

/// devman_executable finds the CLI binary.
///
/// DEVMAN_BIN wins, then a binary shipped next to this application, then PATH.
/// The bundled-next-to-the-app case is what makes an installed DevMan work for a
/// user whose PATH the desktop session never picked up.
fn devman_executable() -> Result<PathBuf, String> {
    if let Some(explicit) = std::env::var_os("DEVMAN_BIN") {
        let path = PathBuf::from(explicit);
        if path.is_file() {
            return Ok(path);
        }
    }

    let names: &[&str] = if cfg!(windows) {
        &["devman.exe"]
    } else {
        &["devman"]
    };

    if let Ok(current) = std::env::current_exe() {
        if let Some(dir) = current.parent() {
            for name in names {
                let candidate = dir.join(name);
                if candidate.is_file() {
                    return Ok(candidate);
                }
            }
        }
    }

    if let Some(path_var) = std::env::var_os("PATH") {
        for dir in std::env::split_paths(&path_var) {
            for name in names {
                let candidate = dir.join(name);
                if candidate.is_file() {
                    return Ok(candidate);
                }
            }
        }
    }

    Err("cannot find the devman executable; install DevMan or set DEVMAN_BIN".to_string())
}

/// spawn_detached starts the daemon without tying it to this window's lifetime.
fn spawn_detached(executable: &Path) -> Result<(), String> {
    let mut command = Command::new(executable);
    command.arg("daemon").arg("start").arg("--foreground");
    command.stdin(std::process::Stdio::null());
    command.stdout(std::process::Stdio::null());
    command.stderr(std::process::Stdio::null());

    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        // DETACHED_PROCESS | CREATE_NO_WINDOW: the daemon must outlive the GUI
        // and must not flash a console window when the GUI starts it.
        const DETACHED_PROCESS: u32 = 0x0000_0008;
        const CREATE_NO_WINDOW: u32 = 0x0800_0000;
        command.creation_flags(DETACHED_PROCESS | CREATE_NO_WINDOW);
    }

    command
        .spawn()
        .map(|_| ())
        .map_err(|err| format!("cannot start the DevMan daemon: {err}"))
}

#[tauri::command]
fn start_daemon() -> Result<Endpoint, String> {
    if let Ok(endpoint) = read_endpoint() {
        return Ok(endpoint);
    }
    let executable = devman_executable()?;
    spawn_detached(&executable)?;

    let deadline = Instant::now() + Duration::from_secs(20);
    let mut last = String::from("the DevMan daemon did not become ready");
    while Instant::now() < deadline {
        match read_endpoint() {
            Ok(endpoint) => return Ok(endpoint),
            Err(err) => last = err,
        }
        std::thread::sleep(Duration::from_millis(200));
    }
    Err(last)
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_dialog::init())
        .invoke_handler(tauri::generate_handler![
            daemon_endpoint,
            start_daemon,
            devman_paths
        ])
        .run(tauri::generate_context!())
        .expect("error while running the DevMan desktop shell");
}
