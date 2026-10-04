// Shared by the build script and unit tests. Keep this validation in sync with
// releaseRepository in scripts/release-updates.mjs.
pub fn release_repository(
    explicit: Option<&str>,
    github: Option<&str>,
    default: &str,
) -> Result<String, &'static str> {
    let slug = explicit.or(github).unwrap_or(default).trim();
    let mut parts = slug.split('/');
    let owner = parts.next().unwrap_or_default();
    let repository = parts.next().unwrap_or_default();
    let valid_owner = !owner.is_empty()
        && owner.len() <= 39
        && owner
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || c == b'-')
        && owner
            .as_bytes()
            .first()
            .is_some_and(u8::is_ascii_alphanumeric)
        && owner
            .as_bytes()
            .last()
            .is_some_and(u8::is_ascii_alphanumeric);
    let valid_repository = !repository.is_empty()
        && repository.len() <= 100
        && !matches!(repository, "." | "..")
        && repository
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, b'_' | b'.' | b'-'));
    if !valid_owner || !valid_repository || parts.next().is_some() {
        return Err("release repository must be a GitHub owner/repository slug");
    }
    Ok(slug.to_owned())
}

#[cfg(test)]
mod tests {
    use super::release_repository;

    #[test]
    fn repository_precedence_is_explicit_then_actions_then_local_config() {
        assert_eq!(
            release_repository(
                Some("fork/desktop"),
                Some("upstream/desktop"),
                "local/desktop"
            )
            .unwrap(),
            "fork/desktop"
        );
        assert_eq!(
            release_repository(None, Some("fork/desktop"), "local/desktop").unwrap(),
            "fork/desktop"
        );
        assert_eq!(
            release_repository(None, None, " local/desktop ").unwrap(),
            "local/desktop"
        );
    }

    #[test]
    fn invalid_explicit_repository_never_falls_back_to_upstream() {
        for slug in [
            "",
            " ",
            "https://github.com/fork/desktop",
            "fork/desktop/extra",
            "../desktop",
            "fork/..",
            "fork/.",
            "-fork/desktop",
            "fork-/desktop",
            "fork_name/desktop",
            "fork/desktop?tag=v1",
            "fork/desktop#fragment",
            "fork/desk%2Ftop",
            "fork/desk\ntop",
            "fork/桌面",
        ] {
            assert!(
                release_repository(Some(slug), Some("upstream/desktop"), "local/desktop").is_err(),
                "accepted {slug:?}"
            );
        }
        assert!(release_repository(
            Some(&format!("{}/desktop", "a".repeat(40))),
            None,
            "local/desktop"
        )
        .is_err());
        assert!(release_repository(
            Some(&format!("fork/{}", "a".repeat(101))),
            None,
            "local/desktop"
        )
        .is_err());
        for slug in [
            "lauchiwa/AstrLink",
            "Calcium-Ion/AstrLink",
            "a/.github",
            "a/repo_name-1.2",
        ] {
            assert_eq!(
                release_repository(Some(slug), None, "local/desktop").unwrap(),
                slug
            );
        }
    }
}
