const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const base = __dirname;
const packages = [
 '/home/nate/Oxy/Kaana/.worktrees/1572-score-distribution-normalization-20261005/tools/contract/node_modules/@oxy.so/contracts',
 '/home/nate/Oxy/Mention/.worktrees/1572-native-second-source-20261005/node_modules/@oxy.so/contracts',
];
for (const pkg of packages) {
 const version = JSON.parse(fs.readFileSync(path.join(pkg,'package.json'),'utf8')).version;
 const {decisionAnswerSchema} = require(path.join(pkg,'dist/cjs/index.js'));
 let positives=0, negatives=0;
 for(const name of ['mass_below_one','mass_above_one','exact_mass']) {
  const answers=JSON.parse(fs.readFileSync(path.join(base,'actual-wire-output',name+'.json'),'utf8'));
  for(const answer of answers) {assert.equal(decisionAnswerSchema.safeParse(answer).success,true); positives++;}
  const score=answers[0];
  const wrongScore={...score,mean:3.24,reply:3.24};
  // The actual wire representation uses an unwrapped scalar reply.
  assert.equal(decisionAnswerSchema.safeParse(wrongScore).success,false); negatives++;
  if(name==='mass_below_one') {
   assert.equal(decisionAnswerSchema.safeParse({...score,distribution:[0,.25,0,0,.7499995]}).success,false);negatives++;
  }
 }
 console.log(JSON.stringify({version,actualAdapterAnswersAccepted:positives,inconsistentAnswersRejected:negatives}));
}
