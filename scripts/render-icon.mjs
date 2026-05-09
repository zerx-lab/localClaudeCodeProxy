// 临时脚本：把 build/appicon.svg 渲染成 build/appicon.png（1024x1024）。
// 用法：在仓库根目录执行 `node scripts/render-icon.mjs`。
// 仅在更新 logo 时手动跑一次；之后再执行 `task common:generate:icons` 让 wails 生成 ico/icns。
import { readFileSync, writeFileSync } from "node:fs";
import { resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import sharp from "sharp";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, "..");
const svgPath = resolve(repoRoot, "build/appicon.svg");
const pngPath = resolve(repoRoot, "build/appicon.png");

const svg = readFileSync(svgPath);
const png = await sharp(svg, { density: 384 })
  .resize(1024, 1024)
  .png({ compressionLevel: 9 })
  .toBuffer();

writeFileSync(pngPath, png);
console.log(`wrote ${pngPath} (${png.length} bytes)`);
