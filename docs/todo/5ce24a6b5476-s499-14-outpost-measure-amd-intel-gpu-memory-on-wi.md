---
id: 5ce24a6b5476
kind: bug
title: 'S499.14 outpost: measure AMD/Intel GPU memory on Windows (not the 4 GiB-capped WMI AdapterRAM) so dhnt.io/vram is advertised; vendor-neutral GPU workers (NVIDIA and AMD)'
seq: 2723
status: done
priority: p0
created: 2026-10-24T20:01:45.656169Z
weave: 95
assignee: agy-gemini3.8-flash
sprint: 507
sprint_id: 77d7ab0f-6aef-5bd3-876d-3f50e9b585ea
sprint_title: 'YC demo blockers: four bug fixes (dks reattach, executor image default, AMD VRAM on Windows, kubectl /c/ paths)'
closed: 2026-10-26T06:16:27.08891Z
closed_by: s507-manager
---

Found 2026-10-24 joining elephant (AMD Ryzen AI MAX+ 395, Radeon 8060S, ~64 GB unified memory) to zoo as a GPU worker. Outpost labels gpu-kind=amd from WMI, but Win32_VideoController.AdapterRAM caps at 4 GiB, so sysinfo_windows reports VRAM unknown and the vk node advertises no dhnt.io/vram; VRAM-requesting work cannot schedule there. NVIDIA works via nvidia-smi (S451). Fix: read dedicated/shared GPU memory via DXGI (IDXGIAdapter3 QueryVideoMemoryInfo / DXGI_ADAPTER_DESC) or the driver registry value HardwareInformation.qwMemorySize, report unified-memory iGPUs honestly (shared budget), and keep nvidia.com/gpu NVIDIA-only. Acceptance: elephant-vk-native advertises dhnt.io/vram with a measured value and a VRAM-requesting job schedules there; NVIDIA path unchanged. Coordinate with Lane 2 (outpost).
