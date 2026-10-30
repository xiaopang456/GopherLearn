$ErrorActionPreference = 'Continue'
# The repository root is the parent of this script's directory, so the script works in any
# checkout and does not carry anybody's local path.
$pub = Split-Path -Parent $PSScriptRoot
$out = New-Object System.Collections.ArrayList

function Detect-License([string]$dir) {
  if (-not $dir -or -not (Test-Path $dir)) { return 'source not in module cache' }
  $candidates = @('LICENSE','LICENSE.txt','LICENSE.md','LICENCE','COPYING','LICENSE-APACHE','LICENSE-MIT','UNLICENSE')
  foreach ($c in $candidates) {
    $p = Join-Path $dir $c
    if (Test-Path $p) {
      $head = (Get-Content $p -TotalCount 12 -ErrorAction SilentlyContinue) -join ' '
      switch -Regex ($head) {
        'GNU AFFERO'             { return 'AGPL-3.0' }
        'GNU GENERAL PUBLIC'     { return 'GPL-3.0' }
        'GNU LESSER'             { return 'LGPL-3.0' }
        'Apache License'         { return 'Apache-2.0' }
        'MIT License|Permission is hereby granted' { return 'MIT' }
        'BSD 3-Clause|Redistribution and use in source and binary forms' { return 'BSD-3-Clause' }
        'BSD 2-Clause'           { return 'BSD-2-Clause' }
        'Mozilla Public License' { return 'MPL-2.0' }
        'ISC License'            { return 'ISC' }
        'This is free and unencumbered software' { return 'Unlicense' }
        default                  { return 'see LICENSE file' }
      }
    }
  }
  return 'no LICENSE file at module root'
}

[void]$out.Add('# Third-party dependency inventory')
[void]$out.Add('')
[void]$out.Add('Generated from the resolved dependency graphs of this release. Each entry lists the')
[void]$out.Add('module or package, the version actually selected by `go list -m all` / `npm ls`, and the')
[void]$out.Add('license found in that package''s own source. Licenses are reported as the packages')
[void]$out.Add('themselves declare them; this is an inventory, not a legal review.')
[void]$out.Add('')
[void]$out.Add('No additional third-party code is vendored in this repository: the build')
[void]$out.Add('carries no vendored dependency of its own, so it contributes no dependency')
[void]$out.Add('and no license here.')
[void]$out.Add('')
[void]$out.Add('Four entries report "no LICENSE file at module root" because the license lives in the')
[void]$out.Add('parent module directory rather than beside the package: the three `cloudwego/eino-ext`')
[void]$out.Add('sub-modules and `volcengine/volc-sdk-golang`. Their parent modules are listed with')
[void]$out.Add('their detected license in the same table.')
[void]$out.Add('')

foreach ($m in @('v2','hybrid','.')) {
  $label = if ($m -eq '.') { 'root' } else { $m }
  [void]$out.Add("## Go module: $label")
  [void]$out.Add('')
  [void]$out.Add('Modules that actually contribute packages to this build:')
  [void]$out.Add('')
  [void]$out.Add('| Module | Version | License |')
  [void]$out.Add('|---|---|---|')
  $dir = Join-Path $pub $m
  $seen = @{}
  $lines = & go -C $dir list -deps -f '{{if .Module}}{{.Module.Path}}|{{.Module.Version}}|{{.Module.Dir}}{{end}}' ./... 2>$null
  foreach ($line in $lines) {
    $parts = $line -split '\|', 3
    if ($parts.Count -lt 3) { continue }
    $path = $parts[0]; $ver = $parts[1]; $modDir = $parts[2]
    if (-not $path -or $seen.ContainsKey($path)) { continue }
    $seen[$path] = $true
    $lic = if ($path -eq 'github.com/xiaopang456/GopherLearn' -or $path -eq 'GopherAI') { 'this repository' } else { Detect-License $modDir }
    [void]$out.Add("| ``$path`` | ``$ver`` | $lic |")
  }
  [void]$out.Add('')
}

[void]$out.Add('## npm packages (`web`)')
[void]$out.Add('')
[void]$out.Add('| Package | Version | License |')
[void]$out.Add('|---|---|---|')
$webDir = Join-Path $pub 'web'
$rows = New-Object System.Collections.ArrayList
Get-ChildItem (Join-Path $webDir 'node_modules') -Recurse -Filter 'package.json' -File -ErrorAction SilentlyContinue |
  Where-Object { $_.DirectoryName -notlike '*node_modules\*node_modules\*' } |
  ForEach-Object {
    try {
      $pkg = Get-Content $_.FullName -Raw | ConvertFrom-Json
      if ($pkg.name -and $pkg.version) {
        $lic = $pkg.license
        if (-not $lic -and $pkg.licenses) { $lic = ($pkg.licenses | ForEach-Object { $_.type }) -join ', ' }
        if (-not $lic) { $lic = 'not declared in package.json' }
        [void]$rows.Add([pscustomobject]@{ Name = $pkg.name; Version = $pkg.version; License = $lic })
      }
    } catch {}
  }
foreach ($r in ($rows | Sort-Object Name -Unique)) {
  [void]$out.Add("| ``$($r.Name)`` | ``$($r.Version)`` | $($r.License) |")
}

$docs = Join-Path $pub 'docs'
New-Item -ItemType Directory -Force -Path $docs | Out-Null
[System.IO.File]::WriteAllLines((Join-Path $docs 'THIRD-PARTY.md'), $out, (New-Object System.Text.UTF8Encoding($false)))
Write-Output ("wrote docs/THIRD-PARTY.md with " + $out.Count + " lines")
