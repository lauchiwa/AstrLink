mod release_repository;

fn main() {
    println!("cargo:rerun-if-changed=../release-repository.json");
    println!("cargo:rerun-if-changed=release_repository.rs");
    println!("cargo:rerun-if-env-changed=ASTRLINK_RELEASE_REPOSITORY");
    println!("cargo:rerun-if-env-changed=GITHUB_REPOSITORY");
    let configuration: serde_json::Value =
        serde_json::from_str(include_str!("../release-repository.json"))
            .expect("invalid release repository configuration");
    let repository = release_repository::release_repository(
        std::env::var("ASTRLINK_RELEASE_REPOSITORY").ok().as_deref(),
        std::env::var("GITHUB_REPOSITORY").ok().as_deref(),
        configuration["repository"]
            .as_str()
            .expect("release repository configuration requires a repository slug"),
    )
    .expect("invalid release repository");
    println!("cargo:rustc-env=ASTRLINK_RELEASE_REPOSITORY={repository}");
    println!(
        "cargo:rustc-env=ASTRLINK_RELEASE_API=https://api.github.com/repos/{repository}/releases"
    );
    println!("cargo:rerun-if-env-changed=TAURI_UPDATER_PUBLIC_KEY");
    let update_key = std::env::var("TAURI_UPDATER_PUBLIC_KEY").unwrap_or_default();
    assert!(
        !update_key.contains(['\n', '\r']),
        "updater public key must be a single base64 line"
    );
    println!(
        "cargo:rustc-env=TAURI_UPDATER_PUBLIC_KEY={}",
        update_key.trim()
    );
    let attributes = tauri_build::Attributes::new();
    let attributes = if std::env::var("CARGO_CFG_TARGET_ENV").as_deref() == Ok("msvc") {
        let manifest =
            std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("windows-app-manifest.xml");
        // tauri-build links its manifest only into bins, leaving lib unit tests without v6 controls.
        println!("cargo:rerun-if-changed={}", manifest.display());
        println!("cargo:rustc-link-arg=/MANIFEST:EMBED");
        println!("cargo:rustc-link-arg=/MANIFESTINPUT:{}", manifest.display());
        attributes.windows_attributes(tauri_build::WindowsAttributes::new_without_app_manifest())
    } else {
        attributes
    };
    tauri_build::try_build(attributes).expect("failed to build Tauri application");
}
