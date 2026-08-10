#requires -Version 7
# ============================================================================
# build_qdao_v3.ps1
# ----------------------------------------------------------------------------
# Builds the qdao FairyGUI package from zero -- no git, no stale XMLs. The
# screens are recomposed from the source images under client/image/ and visually track
# q_daoist_login_clear_2560x1080.png while keeping live FairyGUI controls.
# Uses the 12 v3 UI atoms from
#   client/image/q_daoist_login_ui_10240_redraw_clear_final_layers/q_daoist_login_buttons_redrawn_atomic/
# wired into a minimal Login / Servers / Scene flow.
#
# Idempotent: re-running wipes & rebuilds the qdao package only. Leaves the
# Unity-side mirror under client/unity/Assets/Resources/UI/qdao_v3art/ alone
# (that's used by C# V3Art for character/weapon icons, not by FairyGUI).
#
# After this script:
#   1) Open client/fairygui/qdao/qdao.fairy in FairyGUI Editor
#   2) Press F9 (publish) -- writes new qdao_fui.bytes + qdao atlas PNGs to
#      client/unity/Assets/Resources/UI/qdao/
#   3) Run Unity — should boot straight into the V3 login screen.
# ============================================================================
[CmdletBinding()]
param(
  [string]$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path,
  [switch]$IncludeGalleryAssets
)

$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Drawing

# --- Paths -----------------------------------------------------------------
$srcImage    = Join-Path $RepoRoot 'client\image'
$srcUiAtoms  = Join-Path $srcImage 'q_daoist_login_ui_10240_redraw_clear_final_layers\q_daoist_login_buttons_redrawn_atomic'
$srcWeaponsA = Join-Path $srcUiAtoms 'qstyle_redrawn_600x600\west_eight_immortals_redrawn_100_600x600'
$srcWeaponsB = Join-Path $srcUiAtoms 'qstyle_redrawn_600x600\character_artifacts_redrawn_24_600x600'
$srcCharPack = Join-Path $srcImage 'q_daoist_character_pack_4096'
$pkgRoot     = Join-Path $RepoRoot 'client\fairygui\qdao'
$pkgAssets   = Join-Path $pkgRoot 'assets\qdao'
$pkgUiDir    = Join-Path $pkgAssets 'v3\ui'
$pkgBgDir    = Join-Path $pkgAssets 'v3\bg'
$pkgCharDir  = Join-Path $pkgAssets 'v3\characters'
$pkgWpnDir   = Join-Path $pkgAssets 'v3\icons_weapon'
$pkgCommon   = Join-Path $pkgAssets 'common'
$unityPub    = Join-Path $RepoRoot 'client\unity\Assets\Resources\UI\qdao'

if (-not (Test-Path $srcUiAtoms)) { throw "v3 UI atom source not found: $srcUiAtoms" }

# --- Wipe + recreate -------------------------------------------------------
if (Test-Path $pkgRoot) {
    Write-Host "Removing existing $pkgRoot ..."
    Remove-Item $pkgRoot -Recurse -Force
}
New-Item -ItemType Directory -Path $pkgUiDir,$pkgBgDir,$pkgCommon -Force | Out-Null
if ($IncludeGalleryAssets) {
  New-Item -ItemType Directory -Path $pkgCharDir,$pkgWpnDir -Force | Out-Null
}

# Wipe stale published FairyGUI binary from Unity Resources (only the .bytes
# + atlas PNG, NOT the v3art mirror folder that C# V3Art helper uses).
if (Test-Path $unityPub) {
    Get-ChildItem $unityPub -File -ErrorAction SilentlyContinue | Where-Object {
    $_.Name -in @('qdao_fui.bytes','qdao_fui.bytes.meta','qdao0.png','qdao0.png.meta','qdao0!a.png','qdao0!a.png.meta') -or
    $_.Name -like 'qdao_atlas*.png' -or $_.Name -like 'qdao_atlas*.png.meta'
    } | Remove-Item -Force
}

function Write-Utf8NoBom([string]$path, [string]$content) {
    [System.IO.File]::WriteAllText($path, $content, [System.Text.UTF8Encoding]::new($false))
}

function Get-PngSize([string]$path) {
    $img = [System.Drawing.Image]::FromFile($path)
    try {
        return @{ W = $img.Width; H = $img.Height }
    } finally { $img.Dispose() }
}

# Downscale a PNG in-place so that neither side exceeds $maxDim. Preserves
# aspect ratio with bicubic filtering. Returns @{ W; H; Scale } where Scale
# is the linear factor applied (1.0 = untouched). The atlas packer in the
# FairyGUI editor caps individual textures at 2048x2048, so we use 1024 as a
# safe headroom — even the wide card art (2606x574) becomes 1024x226 which
# packs fine alongside the other sprites in a single qdao0.png.
function Resize-PngIfTooLarge([string]$path, [int]$maxDim) {
    $img = [System.Drawing.Image]::FromFile($path)
    try {
        if ($img.Width -le $maxDim -and $img.Height -le $maxDim) {
            return @{ W = $img.Width; H = $img.Height; Scale = 1.0 }
        }
        $scale = [Math]::Min($maxDim / [double]$img.Width, $maxDim / [double]$img.Height)
        $nw = [Math]::Max(1, [int][Math]::Round($img.Width  * $scale))
        $nh = [Math]::Max(1, [int][Math]::Round($img.Height * $scale))
        $bmp = New-Object System.Drawing.Bitmap $nw, $nh
        $g = [System.Drawing.Graphics]::FromImage($bmp)
        try {
            $g.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic
            $g.SmoothingMode     = [System.Drawing.Drawing2D.SmoothingMode]::HighQuality
            $g.PixelOffsetMode   = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
            $g.CompositingQuality= [System.Drawing.Drawing2D.CompositingQuality]::HighQuality
            $g.DrawImage($img, 0, 0, $nw, $nh)
        } finally { $g.Dispose() }
        $img.Dispose(); $img = $null
        $bmp.Save($path, [System.Drawing.Imaging.ImageFormat]::Png)
        $bmp.Dispose()
        return @{ W = $nw; H = $nh; Scale = $scale }
    } finally { if ($img -ne $null) { $img.Dispose() } }
}

# 9grid coords ("L,T,W,H" inner rect, in source pixels) must follow the
# image scale so the stretchable region still aligns with the art seams.
function Scale-NineGrid([string]$nine, [double]$scale) {
    if (-not $nine) { return '' }
    if ([Math]::Abs($scale - 1.0) -lt 1e-6) { return $nine }
    $parts = $nine.Split(',') | ForEach-Object { [int][Math]::Round([double]$_ * $scale) }
    return ($parts -join ',')
}

# Max per-side pixel cap. 1024 keeps every individual sprite well below the
# editor's 2048 atlas size while leaving 4x area headroom for packing.
$MaxPngDim = 1024

# --- Asset registry --------------------------------------------------------
# Each row: source filename → (id, output name, optional 9-grid string).
# IDs are hand-assigned 8-hex starting at 00000001 to keep the package
# manifest stable and readable. 9-grid is "L,T,R,B" of the stretchable rect.
$uiAtoms = @(
    @{ Src='top_button_green_selected.png';            Id='00010001'; Out='btn_green.png';        Nine='42,28,294,72'  },
    @{ Src='list_bg_unselected.png';                   Id='00010002'; Out='list_idle.png';        Nine='32,20,296,52'  },
    @{ Src='list_bg_selected_green.png';               Id='00010003'; Out='list_active.png';      Nine='32,20,296,52'  },
    @{ Src='server_card_bg_medium.png';                Id='00010004'; Out='card_med_idle.png';    Nine='40,28,440,70'  },
    @{ Src='server_card_bg_medium_selected_green.png'; Id='00010005'; Out='card_med_active.png';  Nine='40,28,440,70'  },
    @{ Src='server_card_bg_wide.png';                  Id='00010006'; Out='card_wide_idle.png';   Nine='40,28,720,70'  },
    @{ Src='server_card_bg_wide_selected_green.png';   Id='00010007'; Out='card_wide_active.png'; Nine='40,28,720,70'  },
    @{ Src='bottom_bar_bg_green_white.png';            Id='00010008'; Out='bar.png';              Nine='80,20,1120,38' },
    @{ Src='search_box_with_icon.png';                 Id='00010009'; Out='input.png';            Nine='88,22,322,40'  },
    @{ Src='ornament_gold_flower.png';                 Id='0001000a'; Out='ornament.png';         Nine=''              },
    @{ Src='status_red_dot.png';                       Id='0001000b'; Out='status_red.png';       Nine=''              },
    @{ Src='ai_qstyle_badges_sheet_chroma.png';        Id='0001000c'; Out='badges_sheet.png';     Nine=''              }
)

# --- Copy PNGs + write per-asset metadata XMLs -----------------------------
$resourceLines = New-Object System.Collections.Generic.List[string]
foreach ($a in $uiAtoms) {
    $srcPath = Join-Path $srcUiAtoms $a.Src
    if (-not (Test-Path $srcPath)) { throw "missing source PNG: $srcPath" }
    $outRelDir = 'v3/ui'
    $outDiskDir = $pkgUiDir
    $outDiskPath = Join-Path $outDiskDir $a.Out
    Copy-Item $srcPath $outDiskPath -Force
    $r = Resize-PngIfTooLarge -path $outDiskPath -maxDim $MaxPngDim
    $w = $r.W; $h = $r.H; $s = $r.Scale
    $nine = Scale-NineGrid $a.Nine $s
    if ($s -lt 1.0) {
        Write-Host ("  copied {0,-40}  {1}x{2}  (scaled x{3:N3})" -f $a.Out, $w, $h, $s)
    } else {
        Write-Host ("  copied {0,-40}  {1}x{2}" -f $a.Out, $w, $h)
    }

    Copy-Item $outDiskPath (Join-Path $pkgAssets $a.Out) -Force

    # Per-asset .png.xml meta — FairyGUI editor respects pre-existing metas
    # rather than auto-assigning new IDs when it opens the package. Write both
    # the organized v3 path and a flat root-path meta; the package manifest uses
    # the flat form because older FairyGUI Editor builds are picky about nested
    # generated assets.
    $nineAttr = if ($nine) { " scale=`"9grid`" scale9grid=`"$nine`"" } else { '' }
    $metaXml = "<?xml version=`"1.0`" encoding=`"utf-8`"?>`n" +
               "<image id=`"$($a.Id)`" name=`"$($a.Out)`" path=`"/$outRelDir/`" exported=`"true`" size=`"$w,$h`"$nineAttr qualityOption=`"source`"/>`n"
    Write-Utf8NoBom (Join-Path $outDiskDir ($a.Out + '.xml')) $metaXml
    $flatMetaXml = "<?xml version=`"1.0`" encoding=`"utf-8`"?>`n" +
             "<image id=`"$($a.Id)`" name=`"$($a.Out)`" path=`"/`" exported=`"true`" size=`"$w,$h`"$nineAttr qualityOption=`"source`"/>`n"
    Write-Utf8NoBom (Join-Path $pkgAssets ($a.Out + '.xml')) $flatMetaXml

    $resourceLines.Add("    <image id=`"$($a.Id)`" name=`"$($a.Out)`" path=`"/`" exported=`"true`" size=`"$w,$h`"$nineAttr qualityOption=`"source`"/>")
}

# --- Backgrounds (full-screen art) ----------------------------------------
# Background source can be 2560x1080 or higher (for example 5120x2160).
# Cap at 4096 so the 2560x1080 runtime view does not upscale a 2048-wide
# published background. Unity supports 4096 atlas pages comfortably here.
$MaxBgDim = 4096
$srcSceneBackground = Join-Path $srcImage 'q_daoist_scene_bg_2560x1080.png'
$srcUiBackground = Join-Path $srcImage 'q_daoist_login_ui_uncropped_highres_final_layers\q_daoist_login_background_ui_uncropped_final_5120x2160.png'
$bgAtoms = @(
  @{ Src=$srcSceneBackground; Id='00020001'; Out='bg_scene.png' },
  @{ Src=$srcUiBackground;    Id='00020002'; Out='bg_ui.png' }
)
foreach ($b in $bgAtoms) {
    if (-not (Test-Path $b.Src)) { Write-Host "  [skip-bg] missing: $($b.Src)"; continue }
    $outPath = Join-Path $pkgBgDir $b.Out
    Copy-Item $b.Src $outPath -Force
    $r = Resize-PngIfTooLarge -path $outPath -maxDim $MaxBgDim
    $w = $r.W; $h = $r.H
    Copy-Item $outPath (Join-Path $pkgAssets $b.Out) -Force
    Write-Host ("  copied {0,-40}  {1}x{2}" -f $b.Out, $w, $h)
    $metaXml = "<?xml version=`"1.0`" encoding=`"utf-8`"?>`n" +
               "<image id=`"$($b.Id)`" name=`"$($b.Out)`" path=`"/v3/bg/`" exported=`"true`" size=`"$w,$h`" qualityOption=`"source`"/>`n"
    Write-Utf8NoBom (Join-Path $pkgBgDir ($b.Out + '.xml')) $metaXml
    $flatMetaXml = "<?xml version=`"1.0`" encoding=`"utf-8`"?>`n" +
             "<image id=`"$($b.Id)`" name=`"$($b.Out)`" path=`"/`" exported=`"true`" size=`"$w,$h`" qualityOption=`"source`"/>`n"
    Write-Utf8NoBom (Join-Path $pkgAssets ($b.Out + '.xml')) $flatMetaXml
    $resourceLines.Add("    <image id=`"$($b.Id)`" name=`"$($b.Out)`" path=`"/`" exported=`"true`" size=`"$w,$h`" qualityOption=`"source`"/>")
}

# --- Optional gallery assets ----------------------------------------------
# Character portraits and weapon/relic icons are useful for later role-create
# and inventory screens, but they are excluded from the default FairyGUI
# package. Unity mirrors them separately under Resources/UI/qdao_v3 for lazy
# code-side loading; qdao_fui.bytes should stay focused on the few UI screens.
$MaxCharDim = 512
$charIdNext = 0x00030001
if ($IncludeGalleryAssets -and (Test-Path $srcCharPack)) {
    $charFiles = Get-ChildItem -Path $srcCharPack -Filter '*_transparent_4096.png' -File |
                 Where-Object { $_.Name -notmatch '^00_reference' -and $_.Name -notmatch 'topright_character_full' } |
                 Sort-Object Name
    foreach ($cf in $charFiles) {
        $outName = ($cf.BaseName -replace '_transparent_4096$', '') + '_v3.png'
        $outPath = Join-Path $pkgCharDir $outName
        Copy-Item $cf.FullName $outPath -Force
        $r = Resize-PngIfTooLarge -path $outPath -maxDim $MaxCharDim
        $w = $r.W; $h = $r.H
        $id = ('{0:x8}' -f $charIdNext); $charIdNext++
        $metaXml = "<?xml version=`"1.0`" encoding=`"utf-8`"?>`n" +
                   "<image id=`"$id`" name=`"$outName`" path=`"/v3/characters/`" exported=`"true`" size=`"$w,$h`" qualityOption=`"source`"/>`n"
        Write-Utf8NoBom (Join-Path $pkgCharDir ($outName + '.xml')) $metaXml
        $resourceLines.Add("    <image id=`"$id`" name=`"$outName`" path=`"/v3/characters/`" exported=`"true`" size=`"$w,$h`" qualityOption=`"source`"/>")
    }
    Write-Host ("  characters: copied {0} portraits at <= {1}px" -f $charFiles.Count, $MaxCharDim)
} else {
  Write-Host "  [skip-characters] gallery assets disabled or missing: $srcCharPack"
}

# --- Weapon / relic icons (100 immortals + 24 artifacts, 600 -> 256) ------
# ID space: 0x00040000+. Contact-sheet jpgs and manifest.json are excluded.
$MaxWpnDim = 256
$wpnIdNext = 0x00040001
$wpnCount = 0
if ($IncludeGalleryAssets) {
  foreach ($wroot in @($srcWeaponsA, $srcWeaponsB)) {
    if (-not (Test-Path $wroot)) { Write-Host "  [skip-weapons] missing: $wroot"; continue }
    $files = Get-ChildItem -Path $wroot -Filter '*.png' -File | Sort-Object Name
    foreach ($wf in $files) {
      $outName = $wf.Name
      $outPath = Join-Path $pkgWpnDir $outName
      if (Test-Path $outPath) { continue }  # de-dupe across the two folders
      Copy-Item $wf.FullName $outPath -Force
      $r = Resize-PngIfTooLarge -path $outPath -maxDim $MaxWpnDim
      $w = $r.W; $h = $r.H
      $id = ('{0:x8}' -f $wpnIdNext); $wpnIdNext++
      $metaXml = "<?xml version=`"1.0`" encoding=`"utf-8`"?>`n" +
             "<image id=`"$id`" name=`"$outName`" path=`"/v3/icons_weapon/`" exported=`"true`" size=`"$w,$h`" qualityOption=`"source`"/>`n"
      Write-Utf8NoBom (Join-Path $pkgWpnDir ($outName + '.xml')) $metaXml
      $resourceLines.Add("    <image id=`"$id`" name=`"$outName`" path=`"/v3/icons_weapon/`" exported=`"true`" size=`"$w,$h`" qualityOption=`"source`"/>")
      $wpnCount++
    }
    }
  Write-Host ("  weapons/relics: copied {0} icons at <= {1}px" -f $wpnCount, $MaxWpnDim)
} else {
  Write-Host "  [skip-weapons] gallery assets disabled"
}

# --- Component manifest entries -------------------------------------------
$components = @(
  @{ Id='c0000001'; Name='V3Banner.xml';  Path='/common/'; Export=$true  },
  @{ Id='c0000002'; Name='V3Btn.xml';     Path='/common/'; Export=$true  },
  @{ Id='c0000003'; Name='V3BtnAlt.xml';  Path='/common/'; Export=$true  },
  @{ Id='c0000004'; Name='V3Tab.xml';     Path='/common/'; Export=$true  },
  @{ Id='c0000005'; Name='V3Input.xml';   Path='/common/'; Export=$true  },
  @{ Id='c0000006'; Name='V3Card.xml';    Path='/common/'; Export=$true  },
  @{ Id='a0000001'; Name='LoginV3.xml';   Path='/';        Export=$true  },
  @{ Id='a0000002'; Name='ServersV3.xml'; Path='/';        Export=$true  },
  @{ Id='a0000003'; Name='SceneV3.xml';   Path='/';        Export=$true  }
)
foreach ($c in $components) {
    $exp = if ($c.Export) { 'true' } else { 'false' }
  $resourceLines.Add("    <component id=`"$($c.Id)`" name=`"$($c.Name)`" path=`"$($c.Path)`" exported=`"$exp`"/>")
}

# --- package.xml -----------------------------------------------------------
# Match the legacy qdao source package format used by the FairyGUI editor.
$packageXml = @"
<?xml version="1.0" encoding="utf-8"?>
<packageDescription id="qdao2026">
  <resources>
$( ($resourceLines -join "`n") )
  </resources>
  <publish name="qdao"/>
</packageDescription>
"@
Write-Utf8NoBom (Join-Path $pkgAssets 'package.xml') $packageXml

# --- qdao.fairy ------------------------------------------------------------
$projectId = ([guid]::NewGuid().ToString('N'))
$fairyXml = @"
<?xml version="1.0" encoding="utf-8"?>
<projectDescription id="$projectId" type="Unity" version="3.0"/>
"@
Write-Utf8NoBom (Join-Path $pkgRoot 'qdao.fairy') $fairyXml

# --- settings/ ------------------------------------------------------------
# FairyGUI Editor (recent builds) stores publish target + design resolution
# under settings/*.json instead of (or in addition to) the package.xml
# <publish> element. Without these files the editor may load the project
# but show packages as empty / fail to render previews.
$pkgSettings = Join-Path $pkgRoot 'settings'
New-Item -ItemType Directory -Path $pkgSettings -Force | Out-Null

Write-Utf8NoBom (Join-Path $pkgSettings 'Publish.json') @'
{
  "path": "../../unity/Assets/Resources/UI/qdao",
  "branchPath": "",
  "fileExtension": "bytes",
  "packageCount": 1,
  "compressDesc": true,
  "binaryFormat": true,
  "jpegQuality": 80,
  "compressPNG": false,
  "codeGeneration": {
    "allowGenCode": false,
    "codePath": "",
    "classNamePrefix": "",
    "memberNamePrefix": "",
    "packageName": "",
    "ignoreNoname": true,
    "getMemberByName": true,
    "codeType": ""
  },
  "includeHighResolution": 0,
  "branchProcessing": 0,
  "atlasSetting": {
    "maxSize": 4096,
    "paging": true,
    "sizeOption": "pot",
    "forceSquare": false,
    "allowRotation": true,
    "trimImage": true
  },
  "include2x": false,
  "include3x": false,
  "include4x": false,
  "fileName": "Publish"
}
'@

Write-Utf8NoBom (Join-Path $pkgSettings 'Adaptation.json') @'
{
  "scaleMode": "ScaleWithScreenSize",
  "screenMathMode": "MatchWidthOrHeight",
  "designResolutionX": 2560,
  "designResolutionY": 1080,
  "devices": [
    { "name": "16:9", "resolutionX": 1920, "resolutionY": 1080 },
    { "name": "19.5:9", "resolutionX": 2340, "resolutionY": 1080 },
    { "name": "20:9", "resolutionX": 2400, "resolutionY": 1080 },
    { "name": "4:3", "resolutionX": 1440, "resolutionY": 1080 }
  ],
  "fileName": "Adaptation"
}
'@

Write-Utf8NoBom (Join-Path $pkgSettings 'Common.json') @'
{
  "font": "",
  "fontSize": 24,
  "textColor": "#5a4025",
  "fontAdjustment": false,
  "colorScheme": [
    "Ink #5a4025",
    "Gold #fff0bd",
    "DeepBrown #2c190f",
    "Jade #2f6f65"
  ],
  "fontSizeScheme": [
    "Body 24",
    "Status 22",
    "Button 30"
  ],
  "fontScheme": [
    "Default"
  ],
  "scrollBars": {
    "horizontal": "",
    "vertical": "",
    "defaultDisplay": "hidden"
  },
  "tipsRes": "",
  "buttonClickSound": "",
  "pivot": "default",
  "listClearOnPublish": false,
  "fileName": "Common"
}
'@

Write-Utf8NoBom (Join-Path $pkgRoot 'README.md') @'
# qdao FairyGUI Project

This project is generated by `tools/scripts/build_qdao_v3.ps1` from the art under `client/image/`.

## Runtime Package

Unity loads a single FairyGUI package from `Assets/Resources/UI/qdao`:

```csharp
UIPackage.AddPackage("UI/qdao/qdao")
```

The generated package contains only the UI screens and reusable UI atoms needed by the client flow:

| Component | Purpose |
|---|---|
| `LoginV3` | gateway/account/password login panel |
| `ServersV3` | reference-style server selector |
| `SceneV3` | minimal in-game placeholder |
| `common/V3Tab` | category tab item |
| `common/V3Card` | server row/card item |
| `common/V3Input` | text input box |
| `common/V3Btn`, `common/V3BtnAlt` | primary and secondary buttons |
| `common/V3Banner` | jade/gold title strip |

Character portraits and weapon/relic icons are not included in the default FairyGUI package. They stay in Unity's `Resources/UI/qdao_v3` mirror for lazy code-side loading. To include them in the FairyGUI source package for editor mockups, rerun the script with `-IncludeGalleryAssets`.

## Regenerate

```powershell
pwsh -NoProfile -ExecutionPolicy Bypass -File tools/scripts/build_qdao_v3.ps1
```

The script wipes and rebuilds `client/fairygui/qdao`, and removes stale published `qdao_fui.bytes` / atlas files so the next Unity run cannot accidentally use old art.

## Publish To Unity

FairyGUI Editor free edition does not support CLI publish. Open `qdao.fairy` in FairyGUI Editor and press F9. The publish target is configured as:

```text
client/unity/Assets/Resources/UI/qdao
```

After publish, Unity should contain `qdao_fui.bytes` plus the generated qdao atlas PNG files.
'@

Write-Utf8NoBom (Join-Path $pkgRoot 'open_qdao.ps1') @'
#requires -Version 5
[CmdletBinding()]
param(
  [string]$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path
)

$ErrorActionPreference = 'Stop'

$editor = Join-Path $RepoRoot 'tools\cache\fairygui-editor\FairyGUI-Editor\FairyGUI-Editor\FairyGUI-Editor.exe'
$project = Join-Path $PSScriptRoot 'qdao.fairy'

if (-not (Test-Path $editor)) { throw "FairyGUI Editor not found: $editor" }
if (-not (Test-Path $project)) { throw "qdao project not found: $project" }

Get-Process FairyGUI-Editor -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Process -FilePath $editor -ArgumentList @($project)
'@

Write-Utf8NoBom (Join-Path $pkgRoot 'open_qdao.bat') @'
@echo off
setlocal
cd /d "%~dp0\..\..\.."
pwsh -NoProfile -ExecutionPolicy Bypass -File "client\fairygui\qdao\open_qdao.ps1"
endlocal
'@

# ============================================================================
# Common components
# ============================================================================
# All sizes are chosen to match the v3 PNG art proportions so nothing has to
# be stretched. ButtonAlt is the "secondary" cream variant — its 9-grid is
# identical to the active list row, just exposed as a button.

# V3Btn — primary green CTA, slightly downsized for denser layout.
# centred on the green tile slice; controller toggles down-state offset).
Write-Utf8NoBom (Join-Path $pkgCommon 'V3Btn.xml') @'
<?xml version="1.0" encoding="utf-8"?>
<component size="330,88" extention="Button" opaque="false">
  <controller name="button" pages="up,,down,,over,,disabled," selected="0"/>
  <displayList>
    <image id="n0" name="bg" src="00010001" fileName="v3/ui/btn_green.png" xy="0,0" size="330,88">
      <gearColor controller="button" values="#ffffff,#fff5d4,#ffffff,#666666"/>
    </image>
    <text id="n1" name="title" xy="0,0" size="330,88" fontSize="30" color="#fff5d4" align="center" vAlign="middle" autoSize="none" singleLine="true" text=""/>
    <graph id="n2" name="hit" xy="0,0" size="330,88" type="rect" lineSize="0" lineColor="#00000000" fillColor="#00000000">
    </graph>
  </displayList>
  <Button mode="Common" downEffect="scale" downEffectValue="0.95"/>
</component>
'@

# V3BtnAlt — secondary cream button using list_active art as its background,
# black text. Used for non-primary actions (返回 / 刷新).
Write-Utf8NoBom (Join-Path $pkgCommon 'V3BtnAlt.xml') @'
<?xml version="1.0" encoding="utf-8"?>
<component size="292,76" extention="Button" opaque="false">
  <controller name="button" pages="up,,down,,over,,disabled," selected="0"/>
  <displayList>
    <image id="n0" name="bgIdle" src="00010002" fileName="v3/ui/list_idle.png" xy="0,0" size="292,76">
      <gearDisplay controller="button" pages="0,2,3"/>
    </image>
    <image id="n1" name="bgDown" src="00010003" fileName="v3/ui/list_active.png" xy="0,0" size="292,76">
      <gearDisplay controller="button" pages="1"/>
    </image>
    <text id="n2" name="title" xy="0,0" size="292,76" fontSize="26" color="#3d2914" align="center" vAlign="middle" autoSize="none" singleLine="true" text=""/>
    <graph id="n3" name="hit" xy="0,0" size="292,76" type="rect" lineSize="0" lineColor="#00000000" fillColor="#00000000">
    </graph>
  </displayList>
  <Button mode="Common" downEffect="scale" downEffectValue="0.97"/>
</component>
'@

# V3Tab — Check-mode toggle, used as left-side category tab. checked
# controller swaps idle/active art and title color.
Write-Utf8NoBom (Join-Path $pkgCommon 'V3Tab.xml') @'
<?xml version="1.0" encoding="utf-8"?>
<component size="200,72" extention="Button" opaque="false">
  <controller name="button"  pages="up,,down,,over,,disabled," selected="0"/>
  <controller name="checked" pages="0,,1," selected="0"/>
  <displayList>
    <image id="n0" name="bgIdle" src="00010002" fileName="v3/ui/list_idle.png" xy="0,0" size="200,72">
      <gearDisplay controller="checked" pages="0"/>
    </image>
    <image id="n1" name="bgActive" src="00010003" fileName="v3/ui/list_active.png" xy="0,0" size="200,72">
      <gearDisplay controller="checked" pages="1"/>
    </image>
    <text id="n2" name="title" xy="0,0" size="200,72" fontSize="24" color="#3d2914" align="center" vAlign="middle" autoSize="none" singleLine="true" text="">
      <gearColor controller="checked" values="#3d2914,#fff5d4"/>
    </text>
    <graph id="n3" name="hit" xy="0,0" size="200,72" type="rect" lineSize="0" lineColor="#00000000" fillColor="#00000000">
    </graph>
  </displayList>
  <Button mode="Check" downEffect="scale" downEffectValue="0.97"/>
</component>
'@

# V3Input — single-line text input with built-in magnifier (icon is baked
# into the PNG, no overlay needed). child "edit" is the GTextInput.
Write-Utf8NoBom (Join-Path $pkgCommon 'V3Input.xml') @'
<?xml version="1.0" encoding="utf-8"?>
<component size="420,84" opaque="true">
  <displayList>
    <image id="n0" name="bg" src="00010009" fileName="v3/ui/input.png" xy="0,0" size="420,84"/>
    <text id="n1" name="edit" xy="86,22" size="312,40" fontSize="26" color="#3d2914" autoSize="none" singleLine="true" input="true" prompt="" text=""/>
  </displayList>
</component>
'@

# V3Banner — horizontal bar background only. The title text is added as a
# sibling <text> node on each screen so the screen owns the wording.
Write-Utf8NoBom (Join-Path $pkgCommon 'V3Banner.xml') @'
<?xml version="1.0" encoding="utf-8"?>
<component size="1280,100" opaque="false">
  <displayList>
    <image id="n0" name="bg" src="00010008" fileName="v3/ui/bar.png" xy="0,0" size="1280,100"/>
  </displayList>
</component>
'@

# V3Card — server / announcement row, downsized to make each server entry
# visually lighter in the list. Composed of:
#   bgIdle / bgActive (gated by `checked`)
#   statusDot (gated by `status`: 0=green/hidden, 1=red, 2=hidden)
#   title (top), subtitle (bottom)
Write-Utf8NoBom (Join-Path $pkgCommon 'V3Card.xml') @'
<?xml version="1.0" encoding="utf-8"?>
<component size="500,106" extention="Button" opaque="false">
  <controller name="button"  pages="up,,down,,over,,disabled," selected="0"/>
  <controller name="checked" pages="0,,1," selected="0"/>
  <controller name="status"  pages="0,,1,,2," selected="0"/>
  <displayList>
    <image id="n0" name="bgIdle" src="00010004" fileName="v3/ui/card_med_idle.png" xy="0,0" size="500,106">
      <gearDisplay controller="checked" pages="0"/>
    </image>
    <image id="n1" name="bgActive" src="00010005" fileName="v3/ui/card_med_active.png" xy="0,0" size="500,106">
      <gearDisplay controller="checked" pages="1"/>
    </image>
    <image id="n2" name="statusDot" src="0001000b" fileName="v3/ui/status_red.png" xy="30,50" size="22,22">
      <gearDisplay controller="status" pages="1"/>
    </image>
    <text id="n3" name="title" xy="88,18" size="246,28" fontSize="22" color="#3d2914" autoSize="none" singleLine="true" text="">
      <gearColor controller="checked" values="#3d2914,#9c2b1a"/>
    </text>
    <text id="n4" name="subtitle" xy="88,54" size="238,26" fontSize="18" color="#5a4025" autoSize="none" singleLine="true" text=""/>
    <graph id="n5" name="hit" xy="0,0" size="500,106" type="rect" lineSize="0" lineColor="#00000000" fillColor="#00000000">
    </graph>
  </displayList>
  <Button mode="Check" downEffect="scale" downEffectValue="0.98"/>
</component>
'@

# ============================================================================
# Screens — design space 2560x1080 (matches Theme.Art.ReferenceWidth/Height)
# Layouts follow q_daoist_login_clear_2560x1080.png: scenic background,
# jade/gold top frame, left category rail, two-column server grid, and a bottom
# action strip. Controls remain real FairyGUI nodes so Unity can bind them.
# ============================================================================

# Login: reuse the reference scene as the first-screen visual, then place a
# compact live login form over the central parchment area.
Write-Utf8NoBom (Join-Path $pkgAssets 'LoginV3.xml') @'
<?xml version="1.0" encoding="utf-8"?>
<component size="2560,1080" opaque="false">
  <displayList>
    <image id="n0" name="bg" src="00020001" fileName="v3/bg/bg_scene.png" xy="0,0" size="2560,1080"/>
    <image id="n17" name="uiBg" src="00020002" fileName="v3/bg/bg_ui.png" xy="0,0" size="2560,1080"/>
    <component id="n1" name="banner" src="c0000001" fileName="common/V3Banner.xml" xy="874,72" size="852,96"/>
    <text id="n2" name="bannerTitle" xy="904,66" size="792,108" fontSize="56" color="#3d2914" align="center" vAlign="middle" autoSize="none" singleLine="true" text="问道·登录"/>

    <image id="n3" name="panelTop" src="00010007" fileName="v3/ui/card_wide_active.png" xy="960,322" size="742,148"/>
    <image id="n4" name="panelMid" src="00010006" fileName="v3/ui/card_wide_idle.png" xy="960,462" size="742,148"/>
    <image id="n5" name="panelBot" src="00010006" fileName="v3/ui/card_wide_idle.png" xy="960,602" size="742,148"/>
    <image id="n6" name="ornL" src="0001000a" fileName="v3/ui/ornament.png" xy="910,262" size="112,112"/>
    <image id="n7" name="ornR" src="0001000a" fileName="v3/ui/ornament.png" xy="1656,262" size="112,112"/>

    <text id="n8" name="lblGateway" xy="1008,340" size="164,42" fontSize="27" color="#5a4025" align="left" vAlign="middle" autoSize="none" singleLine="true" text="网关"/>
    <component id="n9" name="inputGateway" src="c0000005" fileName="common/V3Input.xml" xy="1190,318" size="440,88"/>
    <text id="n10" name="lblAccount" xy="1008,480" size="164,42" fontSize="27" color="#5a4025" align="left" vAlign="middle" autoSize="none" singleLine="true" text="账号"/>
    <component id="n11" name="inputAccount" src="c0000005" fileName="common/V3Input.xml" xy="1190,458" size="440,88"/>
    <text id="n12" name="lblPassword" xy="1008,620" size="164,42" fontSize="27" color="#5a4025" align="left" vAlign="middle" autoSize="none" singleLine="true" text="密码"/>
    <component id="n13" name="inputPassword" src="c0000005" fileName="common/V3Input.xml" xy="1190,598" size="440,88"/>

    <image id="n14" name="bottomPane" src="00010008" fileName="v3/ui/bar.png" xy="930,802" size="1128,118"/>
    <text id="n15" name="status" xy="1052,846" size="540,34" fontSize="23" color="#fff0bd" align="left" vAlign="middle" autoSize="none" singleLine="true" text=""/>
    <component id="n16" name="btnEnter" src="c0000002" fileName="common/V3Btn.xml" xy="1726,818" size="236,84">
      <Button title="登录"/>
    </component>
  </displayList>
</component>
'@

# Servers: the first full-screen reference image is used as an editor-visible
# composition layer, then real FairyGUI lists/buttons are placed over it. This
# makes double-click preview useful while keeping Unity's named controls intact.
# Math: 2 cards * 520 + 1 gap * 16 = 1056 grid width
#       4 cards * 140 + 3 gaps * 14 = 602 grid height
Write-Utf8NoBom (Join-Path $pkgAssets 'ServersV3.xml') @'
<?xml version="1.0" encoding="utf-8"?>
<component size="2560,1080" opaque="false">
  <displayList>
    <image id="n0" name="bg" src="00020001" fileName="v3/bg/bg_scene.png" xy="0,0" size="2560,1080"/>
    <image id="n1" name="uiBg" src="00020002" fileName="v3/bg/bg_ui.png" xy="0,0" size="2560,1080"/>

    <component id="n5" name="tabRecent" src="c0000004" fileName="common/V3Tab.xml" xy="696,204" size="258,60"><Button title="最近登录"/></component>
    <component id="n6" name="tabRecommend" src="c0000004" fileName="common/V3Tab.xml" xy="1034,204" size="220,52"><Button title="推荐服务器"/></component>
    <component id="n7" name="tabAll" src="c0000004" fileName="common/V3Tab.xml" xy="1412,204" size="280,52"><Button title="全部区服"/></component>

    <image id="n8" name="search" src="00010009" fileName="v3/ui/input.png" xy="646,312" size="244,64"/>
    <text id="n9" name="searchHint" xy="704,324" size="160,36" fontSize="22" color="#8b7459" align="left" vAlign="middle" autoSize="none" singleLine="true" text="搜索"/>

    <list id="n10" name="tabs" xy="690,384" size="270,450" overflow="hidden" lineGap="14" defaultItem="ui://qdao/V3Tab" selectionMode="Single" align="center">
      <item title="近期"/>
      <item title="推荐"/>
      <item title="全部"/>
      <item title="新服"/>
      <item title="官方"/>
    </list>

    <list id="n11" name="servers" xy="900,304" size="1120,430" layout="pagination" overflow="scroll" lineGap="2" columnGap="48" defaultItem="ui://qdao/V3Card" selectionMode="Single" align="center">
      <item title="云海宗"/>
      <item title="清风谷"/>
      <item title="碧落渊"/>
      <item title="紫霄峰"/>
      <item title="沧浪洲"/>
      <item title="昆仑墟"/>
      <item title="蓬莱岛"/>
      <item title="瀛洲海"/>
    </list>

    <image id="n12" name="bottomPane" src="00010008" fileName="v3/ui/bar.png" xy="936,842" size="1190,112"/>
    <image id="n13" name="bottomOrn" src="0001000a" fileName="v3/ui/ornament.png" xy="628,810" size="112,112"/>
    <text id="n14" name="selectedLabel" xy="1118,855" size="520,42" fontSize="28" color="#fff0bd" align="left" vAlign="middle" autoSize="none" singleLine="true" text="已选择"/>

    <component id="n15" name="btnBack" src="c0000003" fileName="common/V3BtnAlt.xml" xy="656,890" size="260,72">
      <Button title="返回"/>
    </component>
    <component id="n16" name="btnRefresh" src="c0000003" fileName="common/V3BtnAlt.xml" xy="1660,874" size="210,72">
      <Button title="刷新"/>
    </component>
    <component id="n17" name="btnEnter" src="c0000002" fileName="common/V3Btn.xml" xy="1870,868" size="250,78">
      <Button title="进入"/>
    </component>

    <text id="n18" name="status" xy="1118,895" size="520,28" fontSize="22" color="#fff0bd" align="left" vAlign="middle" autoSize="none" singleLine="true" text=""/>
  </displayList>
</component>
'@

# Scene: minimal in-game placeholder. Just a banner + return button.
Write-Utf8NoBom (Join-Path $pkgAssets 'SceneV3.xml') @'
<?xml version="1.0" encoding="utf-8"?>
<component size="2560,1080" opaque="false">
  <displayList>
    <image id="n0" name="bg" src="00020001" fileName="v3/bg/bg_scene.png" xy="0,0" size="2560,1080"/>
    <image id="n7" name="uiBg" src="00020002" fileName="v3/bg/bg_ui.png" xy="0,0" size="2560,1080"/>
    <component id="n1" name="banner" src="c0000001" fileName="common/V3Banner.xml" xy="540,90" size="1480,118"/>
    <text id="n6" name="bannerTitle" xy="540,100" size="1480,98" fontSize="56" color="#3d2914" align="center" vAlign="middle" autoSize="none" singleLine="true" text="问道·主场景"/>

    <image id="n2" name="ornTL" src="0001000a" fileName="v3/ui/ornament.png" xy="450,60" size="120,120"/>
    <image id="n3" name="ornTR" src="0001000a" fileName="v3/ui/ornament.png" xy="1990,60" size="120,120"/>

    <component id="n4" name="btnBack" src="c0000003" fileName="common/V3BtnAlt.xml" xy="1134,924" size="292,76">
      <Button title="返回登录"/>
    </component>

    <text id="n5" name="status" xy="700,1030" size="1160,28" fontSize="20" color="#fff0bd" align="center" vAlign="middle" autoSize="none" singleLine="true" text=""/>
  </displayList>
</component>
'@

# Legacy FairyGUI Editor builds are more reliable when component XML image
# references use package-root filenames. Keep the organized v3 folders on disk
# for humans, but make editor-facing component XML flat.
Get-ChildItem -Path $pkgAssets -Recurse -Filter '*.xml' |
  Where-Object { $_.Name -notlike '*.png.xml' -and $_.Name -ne 'package.xml' } |
  ForEach-Object {
    $text = [System.IO.File]::ReadAllText($_.FullName, [System.Text.Encoding]::UTF8)
    $text = $text.Replace('fileName="v3/ui/', 'fileName="')
    $text = $text.Replace('fileName="v3/bg/', 'fileName="')
    [System.IO.File]::WriteAllText($_.FullName, $text, [System.Text.UTF8Encoding]::new($false))
  }

$sliceScript = Join-Path $PSScriptRoot 'slice_qdao_fgui_assets.py'
if (Test-Path $sliceScript) {
  python $sliceScript
}

Write-Host ""
Write-Host "qdao package rebuilt from zero."
Write-Host "  PNGs : $($uiAtoms.Count) UI atoms under assets/qdao/v3/ui/"
Write-Host "  Comps: 6 under assets/qdao/common/  (V3Banner,V3Btn,V3BtnAlt,V3Tab,V3Input,V3Card)"
Write-Host "  Scrns: 3 under assets/qdao/  (LoginV3,ServersV3,SceneV3)"
Write-Host "  Gallery assets in FairyGUI package: $([bool]$IncludeGalleryAssets)"
Write-Host ""
Write-Host "Next:"
Write-Host "  1. Open client/fairygui/qdao/qdao.fairy in FairyGUI Editor"
Write-Host "  2. F9 to publish (writes qdao_fui.bytes to Unity Resources/UI/qdao)"
Write-Host "  3. Run Unity — should boot into LoginV3"
