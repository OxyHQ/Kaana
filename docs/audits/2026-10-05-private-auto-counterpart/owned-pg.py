import json,os,socket,subprocess,tempfile
from pathlib import Path
P=Path(__file__).parent;WT='/home/nate/Oxy/Kaana/.worktrees/1572-private-auto-counterpart-20261004';BIN='/usr/lib/postgresql/17/bin'
with tempfile.TemporaryDirectory(prefix='kauto-') as folder:
 p=Path(folder);data=p/'data';pid=None;started=False;result=None
 with socket.socket() as s:s.bind(('127.0.0.1',0));port=s.getsockname()[1]
 def run(args,env=None):
  r=subprocess.run(args,capture_output=True,env=env,cwd=WT)
  with (P/'owned-pg.log').open('ab') as f:f.write(r.stdout+r.stderr)
  if r.returncode:raise RuntimeError('owned PG fixture command failed')
  return r
 try:
  run([BIN+'/initdb','-D',str(data),'-A','trust','--no-locale','--encoding=UTF8'])
  run(['openssl','req','-x509','-newkey','rsa:2048','-nodes','-keyout',str(p/'server.key'),'-out',str(p/'server.crt'),'-days','1','-subj','/CN=localhost','-addext','subjectAltName=IP:127.0.0.1,DNS:localhost']);(p/'server.key').chmod(0o600)
  run([BIN+'/pg_ctl','-D',str(data),'-l',str(p/'postgres.log'),'-o',f'-h 127.0.0.1 -p {port} -k {folder} -c ssl=on -c ssl_cert_file={p}/server.crt -c ssl_key_file={p}/server.key','-w','start']);started=True;pid=int((data/'postmaster.pid').read_text().splitlines()[0])
  rows=(data/'postmaster.pid').read_text().splitlines();assert Path(rows[1]).resolve()==data.resolve();assert int(rows[3])==port;assert Path('/proc/'+str(pid)+'/exe').resolve()==Path(BIN+'/postgres').resolve();assert Path('/proc/'+str(pid)).stat().st_uid==os.getuid()
  run([BIN+'/createdb','-h','127.0.0.1','-p',str(port),'kaana_auto_fixture'])
  run([BIN+'/psql','-h','127.0.0.1','-p',str(port),'-d','kaana_auto_fixture','-v','ON_ERROR_STOP=1','-c','CREATE ROLE kaana_runtime NOLOGIN; CREATE ROLE kaana_migrator NOLOGIN; CREATE ROLE kaana_credential_admin NOLOGIN; CREATE ROLE kaana_customer_credential_control NOLOGIN; CREATE ROLE kaana_platform_credential_control NOLOGIN;'])
  env={k:v for k,v in os.environ.items() if k in ('PATH','HOME','LANG','LC_ALL','TMPDIR')};env['KAANA_PRIVATE_AUTO_TEST_URL']=f'postgres://{os.getlogin() if False else "nate"}@127.0.0.1:{port}/kaana_auto_fixture?sslmode=verify-full&sslrootcert={p}/server.crt'
  r=run(['go','test','-race','-count=1','./internal/kaana','-run','^TestPrivateAutoSignedHTTPPermanentSQLChild$','-v'],env)
  result={'testsExit':r.returncode,'ownPid':pid,'port':port,'database':'kaana_auto_fixture','noProductionAccess':True}
 finally:
  if started:run([BIN+'/pg_ctl','-D',str(data),'-m','fast','-w','stop'])
  absent=pid is None or not Path('/proc/'+str(pid)).exists()
  (P/'owned-pg-cleanup.json').write_text(json.dumps({**(result or {}),'pidAbsent':absent,'postmasterPidAbsent':not(data/'postmaster.pid').exists()},indent=2)+'\n')
