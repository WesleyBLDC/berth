# 0020: Test lab in macOS VMs with a berth_lab build flavor

Status: Accepted, 2026-11-19

## Context

`make check` proves the logic with the `berth_e2e` flavor: test trust rules, a fake helper in place of `berth-native`, headless browsers.
Everything past that is a guided manual script the maintainer runs by hand: real signing and code checks, the store dialog and Touch ID, headful browsers against the real pasteboard, the app's GUI, login item, notifications and Screen Recording, and a real second macOS user for E3.
Those scripts gate E2b, E3, M2, M3, M4 and M5 today, and M5's takeover and M6's fills add more of them, since both need headful browsers and approvals.
A person in the loop makes each gate slow, rare and easy to skip.

| Option | Real macOS, signing and GUI | Unattended | Cost |
|---|---|---|---|
| Guided scripts on the maintainer's Mac | Yes | No | His time at every gate |
| macOS VMs with tart and a lab flavor | Yes, with a lab certificate and scripted approvals | Yes | A base image, a runner, a build flavor; 2 VMs at once per Mac |
| Linux containers | No: Berth is macOS only | Yes | Cannot run Berth at all |
| UI automation on the maintainer's own login session | Yes | Partly | Touches his real profile, Keychain and screen |

## Decision

Berth's end to end checks run unattended in macOS VMs on Apple silicon through tart, from a base image built as code, on a clean clone per run, locally (`make lab-test`) and as a self hosted GitHub Actions runner on the maintainer's Mac.

- A new build flavor, `berth_lab`, modelled on `berth_e2e`, is what the VM runs. It changes only two things:
  - the trust requirement is anchored to a lab only self signed certificate authority whose certificate exists only in the image, so real `codesign`, real bundle pinning and real code checks run against a real certificate;
  - `berth-native`'s approval sits behind a seam that reads scripted answers, or Accessibility drives the real password dialog.
- Everything else is the release code: the value channel, the receiver check, `mlock`, the CDP filter, the app pin.
- A release test proves that no `berth_lab` code, certificate or seam reaches a release build, as the `berth_e2e` release tests do today.
- The world outside Berth (HTTP and TLS upstreams, a DNS server, hostile pages, slow and malformed servers) runs as Linux containers on apple/container on the host, never on the internet.
- What still needs a person shrinks to one short release checklist: real Touch ID, the real team signing key in the host keychain, and the maintainer's own Chrome profile.

## Why

A VM is real macOS: launchd, TCC, the window server, the Keychain and `codesign` behave as on the maintainer's Mac, so a test there proves what a guided script proves, without his hands, on every change.
A flavor with a lab certificate keeps the code checks real instead of switching them off, and keeps the difference from a release build to two seams a test can pin.
The costs are honest: no Touch ID or Secure Enclave inside a VM, at most 2 macOS VMs at once per Mac under Apple's licence, SIP turned off in the image to pre grant permissions, and a few hundred gigabytes of images.
The plan with its items is [docs/plans/test-lab.md](../plans/test-lab.md).
