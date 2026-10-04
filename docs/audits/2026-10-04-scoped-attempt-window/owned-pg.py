import os,socket,subprocess,tempfile,json
from pathlib import Path
P=Path(__file__).parent;WT=Path('/home/nate/Oxy/Kaana/.worktrees/1572-scoped-attempt-window-20261004');PG=Path('/usr/lib/postgresql/17/bin')
env={k:v for k,v in os.environ.items() if k in ('PATH','HOME','LANG','LC_ALL','TMPDIR')}
with tempfile.TemporaryDirectory(prefix='kscope-') as folder:
 p=Path(folder);data=p/'data';started=False;pid=None;status=None
 with socket.socket() as s:s.bind(('127.0.0.1',0));port=s.getsockname()[1]
 def run(args,callenv=env):
  result=subprocess.run([str(a) for a in args],cwd=WT,env=callenv,capture_output=True)
  with (P/'owned-pg.log').open('ab') as f:f.write(result.stdout+result.stderr)
  if result.returncode:raise RuntimeError('owned fixture command failed')
  return result
 try:
  run([PG/'initdb','-D',data,'-A','trust','--no-locale','--encoding=UTF8'])
  run(['openssl','req','-x509','-newkey','rsa:2048','-nodes','-keyout',p/'server.key','-out',p/'server.crt','-days','1','-subj','/CN=localhost','-addext','subjectAltName=IP:127.0.0.1,DNS:localhost'])
  (p/'server.key').chmod(0o600)
  run([PG/'pg_ctl','-D',data,'-l',p/'postgres.log','-o',f'-h 127.0.0.1 -p {port} -k {folder} -c ssl=on -c ssl_cert_file={p}/server.crt -c ssl_key_file={p}/server.key','-w','start']);started=True
  pid=int((data/'postmaster.pid').read_text().splitlines()[0]);assert Path(f'/proc/{pid}/exe').resolve()==(PG/'postgres').resolve()
  assert Path((data/'postmaster.pid').read_text().splitlines()[1]).resolve()==data.resolve()
  run([PG/'createdb','-h','127.0.0.1','-p',port,'kaana_scope_fixture'])
  run([PG/'psql','-h','127.0.0.1','-p',port,'-d','kaana_scope_fixture','-v','ON_ERROR_STOP=1','-c','CREATE ROLE kaana_runtime NOLOGIN; CREATE ROLE kaana_migrator NOLOGIN; CREATE ROLE kaana_credential_admin NOLOGIN; CREATE ROLE kaana_customer_credential_control NOLOGIN; CREATE ROLE kaana_platform_credential_control NOLOGIN;'])
  testenv=env|{'KAANA_SCOPED_WINDOW_TEST_URL':f'postgres://nate@127.0.0.1:{port}/kaana_scope_fixture?sslmode=verify-full&sslrootcert={p}/server.crt'}
  r=subprocess.run(['go','test','-race','-count=1','./internal/kaana','-run','^TestScopedExecutorSourceWindowUsesBoundedSQLClaimAndPermanentReplayDenial$','-v'],cwd=WT,env=testenv,capture_output=True)
  with (P/'owned-pg.log').open('ab') as f:f.write(r.stdout+r.stderr)
  status=r.returncode;print(json.dumps({'testsExit':status,'ownPid':pid,'noProductionAccess':True}));raise SystemExit(status)
 finally:
  if started:run([PG/'pg_ctl','-D',data,'-m','fast','-w','stop'])
  absent=pid is None or not Path('/proc/'+str(pid)).exists()
  assert absent and not (data/'postmaster.pid').exists()
  (P/'owned-pg-cleanup.json').write_text(json.dumps({'testsExit':status,'pid':pid,'pidAbsent':absent,'postmasterPidAbsent':not (data/'postmaster.pid').exists(),'tls':'verify-full owned self-signed fixture','noProductionAccess':True},indent=2)+'\n')
