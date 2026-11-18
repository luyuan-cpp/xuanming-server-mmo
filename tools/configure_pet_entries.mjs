// 可复现的宝宝条目首测源表。使用官方 artifact-tool，保留既有 Pet/PetRule 工作簿。
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';
import path from 'node:path';
import fs from 'node:fs/promises';
const root = process.argv[2] || process.cwd();
const modules = process.env.CODEX_BUNDLED_NODE_MODULES || 'C:/Program Files/WindowsApps/OpenAI.CodexPrimaryRuntime.v26-1116-1210-0_26.1116.1210.0_x64__3k8sg7r9htsxt/dependencies/node/node_modules';
const req = createRequire(path.join(modules, '_loader.cjs'));
const {FileBlob, SpreadsheetFile, Workbook} = await import(pathToFileURL(req.resolve('@oai/artifact-tool')));
const rows=[];
const add=(id,name,desc,category,channel,state,rate,icon)=>rows.push([id,name,desc,category,channel,state,20,rate,id,icon]);
add(501,'物理伤害','普通攻击与手动直接物理技能伤害；不提高攻击面板，不作用于反击、追击、反震、分担及混乱强制攻击。',1,1,0,50,'physical');
add(502,'法术伤害','手动直接法术技能伤害；不作用于中毒、治疗、护盾和派生伤害。',1,2,0,50,'magic');
add(503,'治疗效果','自身施放的即时治疗与持续治疗；排除复活初始血量、吸血和蓝血转化。',3,3,0,50,'healing');
add(504,'护盾容量','自身施放的有限数值护盾容量；不增加次数、时长及法力盾换算。',3,4,0,50,'shield');
const ailments=['封主动','封法','封物','混乱','睡眠','冰冻','中毒'];
for(let i=0;i<ailments.length;i++){
 add(511+i,ailments[i]+'命中','仅提高'+ailments[i]+'施加概率；不增加目标、持续时间或伤害。',2,5,i+1,25,i===3?'confusion':'seal');
 add(521+i,'抗'+ailments[i],'仅降低受到'+ailments[i]+'的概率；不是免疫。',2,6,i+1,25,'seal');
}
add(531,'中毒伤害','自己施加的中毒每跳伤害；不改变命中、目标数和持续时间。',1,7,0,50,'magic');
add(532,'劝退命中','仅提高对合法敌方宝宝的劝退概率，不扩大目标范围。',2,8,0,25,'seal');
add(533,'抗劝退','降低被劝退概率；不改变回场等待时间。',2,9,0,25,'seal');
async function make(name,headers,descriptions,data){const w=Workbook.create();const s=w.worksheets.add(name);s.getRange(`A1:${col(headers.length)}1`).values=[headers];s.getRange(`A5:${col(headers.length)}5`).values=[descriptions];s.getRange(`A6:${col(headers.length)}${5+data.length}`).values=data;await(await SpreadsheetFile.exportXlsx(w)).save(path.join(root,'data',name+'.xlsx'));}
function col(n){let s='';while(n){n--;s=String.fromCharCode(65+n%26)+s;n=Math.floor(n/26);}return s;}
await make('PetEntry',['id','name','desc','category','channel','state_kind','cap','per_point_bps','sort','icon'],['稳定方向ID','方向名','范围与排除','1输出2障碍3辅助','结算渠道','障碍类型','单项上限','每点收益万分比','排序','客户端条目图标'],rows);
await make('PetTier',['id','name','art_key','desc'],['独立五阶ID','名称','徽章资源尾名','身份说明'],['普通','变异','灵兽','神兽','圣兽'].map((n,i)=>[i+1,n,['normal','mutant','spirit','divine','sacred'][i],'独立身份；同一20点条目预算；首测不额外叠加全局倍率。']));
for(const [name,range,headers,values] of [['PetRule','G1:I1',['entry_config_version','entry_total_points','entry_max_directions'],[[1,20,0]]],['Pet','Y1:Y1',['cultivation_tier'],Array.from({length:22},()=>[1])]]){
 const w=await SpreadsheetFile.importXlsx(await FileBlob.load(path.join(root,'data',name+'.xlsx')));const s=w.worksheets.getItem(name);s.getRange(range).values=[headers];const start=name==='Pet'?'Y':'G',end=name==='Pet'?'Y':'I';s.getRange(`${start}5:${end}5`).values=[name==='Pet'?['独立五阶；现有种类明确登记普通，不推导旧品质']:['规则版本','统一条目预算','非零方向上限0不限']];s.getRange(`${start}6:${end}${5+values.length}`).values=values;await(await SpreadsheetFile.exportXlsx(w)).save(path.join(root,'data',name+'.xlsx'));
}
console.log(JSON.stringify({entryRows:rows.length,tiers:5,configVersion:1,totalPoints:20,existingPetRowsExplicitTier:1}));

const inspectDir=path.join(root,'build/verification/pet-entries-table-inspect');await fs.mkdir(inspectDir,{recursive:true});
for(const name of ['Pet','PetRule','PetEntry','PetTier']) {try {await fs.rename(path.join(root,'data',name+'.xlsx.inspect.ndjson'),path.join(inspectDir,name+'.xlsx.inspect.ndjson'));}catch(e){if(e.code!=='ENOENT')throw e;}}
