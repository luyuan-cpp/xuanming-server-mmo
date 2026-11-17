// 使用artifact-tool增量编写源表；不改变既有工作表、样式与其它会话的数据。
// python skill_catalog.py plan --output <plan.json> 后运行本脚本。
import fs from 'node:fs/promises';
import path from 'node:path';
import {createRequire} from 'node:module';
import {pathToFileURL, fileURLToPath} from 'node:url';
import {createHash} from 'node:crypto';
import {spawnSync} from 'node:child_process';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const args = process.argv.slice(2);
const previewOnly = args.includes('--preview');
const planPath = args.find(x => !x.startsWith('--'));
if (!planPath || !process.env.ARTIFACT_NODE_MODULES) throw new Error('Usage: ARTIFACT_NODE_MODULES=<bundled node_modules> node skill_catalog_xlsx.mjs <plan.json> [--preview]');
const require = createRequire(path.join(process.env.ARTIFACT_NODE_MODULES, '__skill_loader.cjs'));
const {FileBlob, SpreadsheetFile} = await import(pathToFileURL(require.resolve('@oai/artifact-tool')).href);
const plan = JSON.parse(await fs.readFile(planPath, 'utf8'));
const out = path.dirname(path.resolve(planPath));
const col = i => {let text=''; for (++i;i;i=Math.floor((i-1)/26)) text=String.fromCharCode(65+(i-1)%26)+text; return text;};
const digest = data => createHash('sha256').update(data).digest('hex');
const tables = [...plan.tables, {name:'Pet', rows:[], fields:[]}];

for (const table of tables) {
  const source = path.join(root, 'data', `${table.name}.xlsx`);
  const original = await fs.readFile(source);
  if (table.source_sha256 && digest(original) !== table.source_sha256) throw new Error(`Source changed after planning: ${table.name}`);
  const workbook = await SpreadsheetFile.importXlsx(await FileBlob.load(source));
  const sheet = workbook.worksheets.getItem(table.name);
  if (previewOnly) {
    console.log((await workbook.inspect({kind:'region',sheetId:table.name,range:'A1:H8',maxChars:1500})).ndjson);
    const preview = await workbook.render({sheetName:table.name,range:'A1:H8',scale:1});
    await fs.writeFile(path.join(out, `${table.name}-before.png`), new Uint8Array(await preview.arrayBuffer()));
    continue;
  }
  const current = sheet.getUsedRange().values;
  const headers = [...current[0]];
  while (headers.length && headers.at(-1) == null) headers.pop();
  const cells = {};
  const put = (r,c,value,allowChange=false) => {
    const old = current[r-1]?.[c] ?? null;
    if (old === value || (old == null && value == null)) return;
    if (old != null && !allowChange) throw new Error(`Refusing to replace ${table.name}!${col(c)}${r}`);
    cells[`${col(c)}${r}`] = value;
  };
  for (const field of table.fields) {
    if (headers.includes(field.name)) continue;
    const c=headers.length;
    put(1,c,field.name); put(5,c,field.comment || null);
    headers.push(field.name, ...Array(field.width-1).fill(null));
  }
  const indexes = Object.fromEntries(headers.flatMap((h,i)=>h?[[h,i]]:[]));
  const existing = new Map(current.slice(5).flatMap((row,i)=>row[0]==null?[]:[[row[0],i+6]]));
  let nextRow = Math.max(5,...existing.values())+1;
  for (const row of table.rows) {
    const r=existing.get(row.id) ?? nextRow++;
    for (const [key,value] of Object.entries(row)) {
      if (!(key in indexes)) throw new Error(`Unknown column: ${table.name}.${key}`);
      const values=Array.isArray(value)?value:[value];
      for (let i=0;i<values.length;i++) {
        const known=table.previous_values?.[String(row.id)]?.[key];
        const approved=known!==undefined && current[r-1]?.[indexes[key]+i]===known;
        put(r,indexes[key]+i,values[i],approved);
      }
    }
  }
  if (table.name==='Pet') {
    for (const [id,values] of Object.entries(plan.pet_skills)) {
      const r=existing.get(Number(id));
      if (!r) throw new Error(`Missing pet: ${id}`);
      const old=current[r-1].slice(indexes.skill,indexes.skill+4);
      if (!old.every((v,i)=>v==null||v===0||v===14||v===values[i])) throw new Error(`Pet skills changed concurrently: ${id}`);
      values.forEach((value,i)=>put(r,indexes.skill+i,value,true));
    }
  }
  if (!Object.keys(cells).length) { console.log(`${table.name}: already current`); continue; }
  for (const [address,value] of Object.entries(cells)) sheet.getRange(address).values=[[value]];
  workbook.recalculate();
  const staged=path.join(out,`${table.name}-authored.xlsx`);
  await (await SpreadsheetFile.exportXlsx(workbook)).save(staged);
  const manifest=path.join(out,`${table.name}-cells.json`);
  await fs.writeFile(manifest,JSON.stringify({source,sha256:digest(original),staged,cells}));
  // 仅移植已编辑单元格的值到原OOXML包，保留所有非目标布局和原样式。
  const result=spawnSync(process.env.PYTHON || 'python',[path.join(root,'tools/scripts/skill_catalog_merge.py'),manifest],{encoding:'utf8'});
  if (result.status!==0) throw new Error(result.stderr || result.stdout || 'OOXML cell merge failed');
  console.log(result.stdout.trim());
  const verify=await SpreadsheetFile.importXlsx(await FileBlob.load(source));
  const tail=table.name==='Pet'?'S1:X9':`A${nextRow-4}:H${nextRow-1}`;
  const preview=await verify.render({sheetName:table.name,range:tail,scale:1});
  await fs.writeFile(path.join(out,`${table.name}-after.png`),new Uint8Array(await preview.arrayBuffer()));
}
