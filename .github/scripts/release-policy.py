#!/usr/bin/env python3
"""Classify release tags without publishing anything or consulting GitHub."""

import argparse
import re
import unittest


SEMVER_TAG = re.compile(
    r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)"
    r"(?:-(?P<prerelease>[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?"
    r"(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?"
)


def classify_release_tag(tag):
    match = SEMVER_TAG.fullmatch(tag)
    if not match:
        raise ValueError("Release tag must be a complete v-prefixed semantic version")
    prerelease = match.group("prerelease")
    if prerelease and any(
        part.isdigit() and len(part) > 1 and part.startswith("0")
        for part in prerelease.split(".")
    ):
        raise ValueError("Numeric prerelease identifiers must not have leading zeroes")
    is_prerelease = prerelease is not None
    return {
        "prerelease": is_prerelease,
        "draft": is_prerelease,
        "publish_client_registries": not is_prerelease,
    }


class ReleasePolicyTests(unittest.TestCase):
    def test_rc_is_an_unpublished_prerelease_without_registry_publication(self):
        self.assertEqual(
            classify_release_tag("v0.1.3-rc4.5"),
            {"prerelease": True, "draft": True, "publish_client_registries": False},
        )

    def test_other_semver_prereleases_are_also_held_for_review(self):
        for tag in ("v1.2.3-alpha.1", "v1.2.3-0", "v1.2.3-rc.1+build-123"):
            with self.subTest(tag=tag):
                self.assertTrue(classify_release_tag(tag)["draft"])
                self.assertFalse(classify_release_tag(tag)["publish_client_registries"])

    def test_stable_behavior_is_unchanged_including_metadata_hyphens(self):
        for tag in ("v0.1.2", "v1.2.3", "v0.0.0", "v1.2.3+build-123"):
            with self.subTest(tag=tag):
                self.assertEqual(
                    classify_release_tag(tag),
                    {"prerelease": False, "draft": False, "publish_client_registries": True},
                )

    def test_malformed_or_partial_versions_do_not_publish(self):
        for tag in (
            "", "0.1.3", "v0.1", "v0.1.3.4", "v01.2.3", "v1.2.3-",
            "v1.2.3-rc..1", "v1.2.3-01", "v1.2.3-rc.01", "v1.2.3+",
            "v1.2.3\n", "v1.2.3;echo unsafe", "v1.2.3+metadata/invalid",
        ):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                classify_release_tag(tag)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("tag", nargs="?")
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.self_test:
        result = unittest.TextTestRunner(verbosity=2).run(
            unittest.defaultTestLoader.loadTestsFromTestCase(ReleasePolicyTests)
        )
        return 0 if result.wasSuccessful() else 1
    try:
        policy = classify_release_tag(args.tag or "")
    except ValueError as error:
        parser.error(str(error))
    for key, value in policy.items():
        print(f"{key}={str(value).lower()}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
