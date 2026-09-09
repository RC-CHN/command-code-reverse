#!/usr/bin/env python3
"""Opt-in live compatibility probes. Uses .env keys; incurs small inference usage.
Build proxy first, then: python3 live-check.py --binary /tmp/commandcode-live-proxy --output /tmp/live.json
Only synthetic prompts and redacted summaries are persisted. The local relay
captures application requests; it does not establish TLS/client indistinguishability.
"""
import argparse, base64, copy, hashlib, json, struct, zlib, os, re, secrets, signal, socket, subprocess, threading, time
import urllib.request, urllib.error
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--binary', required=True)
parser.add_argument('--output', required=True)
parser.add_argument('--key-index', type=int, default=1)
parser.add_argument('--only', default='', help='Comma-separated case names')
parser.add_argument('--restart-check', action='store_true')
parser.add_argument('--fixed-test-seed', action='store_true', help='Use a synthetic stable seed, without changing .env')
parser.add_argument('--auth-mode', choices=['managed','passthrough'], default='managed')
parser.add_argument('--max-tokens', type=int, default=96)
parser.add_argument('--suite', choices=['basic','full'], default='full')
args = parser.parse_args()
env = dict(os.environ)
for line in (ROOT/'.env').read_text().splitlines():
    line=line.strip()
    if line and not line.startswith('#') and '=' in line:
        k,v=line.split('=',1)
        if not env.get(k.strip()): env[k.strip()]=v.strip().strip('\"\'')
keys=[k.strip() for k in env['COMMAND_CODE_API_KEY'].split(',') if k.strip()]
key=keys[args.key_index-1]
base=env.get('COMMAND_CODE_API_BASE','https://api.commandcode.ai').rstrip('/')
proxy_key=secrets.token_urlsafe(24)
secret_values=[*keys, proxy_key, env.get('PROXY_API_KEY',''), env.get('FINGERPRINT_SEED','')]
def redact(value):
    value=str(value)
    for secret in secret_values:
        if secret: value=value.replace(secret,'[REDACTED]')
    return re.sub(r'[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}', '[EMAIL]', value)[:600]

captures=[]
class Relay(BaseHTTPRequestHandler):
    protocol_version='HTTP/1.0'
    def log_message(self,*unused): pass
    def do_POST(self):
        data=self.rfile.read(int(self.headers.get('Content-Length','0')))
        capture={'body':json.loads(data),'headers':dict(self.headers),'events':[]}
        captures.append(capture)
        headers={k:v for k,v in self.headers.items() if k.lower() not in ['host','connection','content-length','accept-encoding']}
        headers['Accept-Encoding']='identity'
        req=urllib.request.Request(base+self.path,data=data,headers=headers,method='POST')
        try:
            try: response=urllib.request.urlopen(req,timeout=45)
            except urllib.error.HTTPError as e: response=e
            capture['http']=response.status
            self.send_response(response.status)
            self.send_header('Content-Type',response.headers.get('Content-Type','application/json'))
            self.end_headers()
            raw=bytearray()
            with response:
                while True:
                    block=response.read1(16384)
                    if not block: break
                    if len(raw)<4*1024*1024: raw.extend(block)
                    self.wfile.write(block); self.wfile.flush()
            for line in raw.splitlines():
                try: capture['events'].append(json.loads(line))
                except (ValueError,UnicodeError): pass
        except Exception as e:
            capture['error_class']=type(e).__name__
            try: self.send_error(502)
            except (BrokenPipeError,ConnectionError): pass

relay=ThreadingHTTPServer(('127.0.0.1',0),Relay)
threading.Thread(target=relay.serve_forever,daemon=True).start()
with socket.socket() as sock:
    sock.bind(('127.0.0.1',0)); port=sock.getsockname()[1]
if args.fixed_test_seed: env['FINGERPRINT_SEED']='commandcode-compatibility-live-test-seed'
env.update(COMMAND_CODE_API_BASE=f'http://127.0.0.1:{relay.server_port}',COMMAND_CODE_API_KEY=key,
           PROXY_API_KEY=proxy_key,AUTH_MODE=args.auth_mode,HOST='127.0.0.1',PORT=str(port),
           COMMAND_CODE_VERSION_PIN='1.51.3',FINGERPRINT_ENABLED='false',LOG_LEVEL='error',
           STREAM_IDLE_TIMEOUT_SECONDS='45',NONSTREAM_IDLE_TIMEOUT_SECONDS='45')
report={'date':time.strftime('%Y-%m-%d'), 'key_index':args.key_index, 'suite':args.suite,
        'version':'1.51.3','auth_mode':args.auth_mode,'fixed_seed_configured':bool(env.get('FINGERPRINT_SEED')),'synthetic_test_seed':args.fixed_test_seed,
        'transport':'actual Go proxy through local capture relay to real upstream',
        'cases':[], 'comparisons':{}}
proc=subprocess.Popen([args.binary],cwd=ROOT,env=env,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
url=f'http://127.0.0.1:{port}'
records={}
def save():
    Path(args.output).write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n')
def run(name,body,authorization=True):
    if args.only and name not in args.only.split(','): return {'skipped':True}
    start=time.monotonic(); idx=len(captures)
    headers={'Content-Type':'application/json'}
    if authorization: headers['Authorization']='Bearer '+(proxy_key if args.auth_mode=='managed' else key)
    request=urllib.request.Request(url+'/v1/chat/completions',data=json.dumps(body).encode(),headers=headers)
    try:
        try: response=urllib.request.urlopen(request,timeout=55)
        except urllib.error.HTTPError as e: response=e
        with response: status=response.status; raw=response.read().decode()
        objects=[]
        if raw.startswith('data:'):
            for line in raw.splitlines():
                if line.startswith('data: ') and line!='data: [DONE]':
                    try: objects.append(json.loads(line[6:]))
                    except ValueError: pass
        else:
            try: objects=[json.loads(raw)]
            except ValueError: pass
        errors=[o['error'] for o in objects if isinstance(o,dict) and 'error' in o]
        usages=[o['usage'] for o in objects if isinstance(o,dict) and o.get('usage')]
        text=''; toolcalls=[]; finish=[]
        for obj in objects:
            for choice in obj.get('choices',[]):
                msg=choice.get('message',choice.get('delta',{}))
                text+=msg.get('content') or ''
                toolcalls+=msg.get('tool_calls') or []
                if choice.get('finish_reason'): finish.append(choice['finish_reason'])
        result={'name':name,'http':status,'elapsed_s':round(time.monotonic()-start,2),
                'success':status==200 and not errors and bool(text or toolcalls),
                'text':redact(text),'tool_calls':toolcalls,'finish':finish,'done':raw.endswith('data: [DONE]\n\n'),
                'usage':usages[-1] if usages else None,'errors':[redact(json.dumps(e)) for e in errors],
                'upstream_requests':len(captures)-idx}
        if len(captures)>idx:
            c=captures[idx]; records[name]=c
            h={k.lower():v for k,v in c['headers'].items()}; b=c['body']
            events=c['events']; usage=[e.get('totalUsage',e.get('usage')) for e in events if e.get('type') in ['finish','finish-step']]
            costs=[e.get('providerMetadata',{}).get('gateway',{}).get('cost') for e in events]
            result.update(upstream_http=c.get('http'),body_sha256=hashlib.sha256(json.dumps(b,sort_keys=True).encode()).hexdigest(),
                system_type=type(b['params'].get('system')).__name__,
                system_is_single_space=b['params'].get('system')==' ',
                identity_hashes={n:hashlib.sha256(v.encode()).hexdigest() for n,v in {'thread':b['threadId'],'session':h['x-session-id'],'project':h['x-project-slug']}.items()},
                header_checks={'upstream_key_used':h.get('authorization')=='Bearer '+key,'proxy_key_not_forwarded':proxy_key not in json.dumps(h),
                    'cli_version':h.get('x-command-code-version')=='1.51.3','user_agent':h.get('user-agent')=='cli',
                    'environment':h.get('x-cli-environment')=='production','traceparent':bool(re.fullmatch(r'00-[0-9a-f]{32}-[0-9a-f]{16}-01',h.get('traceparent',''))),
                    'project_slug_matches_working_dir':h.get('x-project-slug')==re.sub('[^a-z0-9]+','-',b['config']['workingDir'].lower()).strip('-')},
                upstream_event_types=sorted({e.get('type','unknown') for e in events}),upstream_usage=usage[-1] if usage else None,
                reported_cost_usd=next((x for x in reversed(costs) if x is not None),None))
        report['cases'].append(result);save()
        print(json.dumps({k:result[k] for k in ['name','http','success','elapsed_s','text','errors','usage']},ensure_ascii=False),flush=True)
        return result
    except Exception as e:
        report['cases'].append({'name':name,'error_class':type(e).__name__,'elapsed_s':round(time.monotonic()-start,2)})
        save();print(json.dumps(report['cases'][-1]),flush=True);return report['cases'][-1]

def compare(a,b):
    if a not in records or b not in records:return
    x,y=records[a],records[b];xh={k.lower():v for k,v in x['headers'].items()};yh={k.lower():v for k,v in y['headers'].items()}
    report['comparisons'][a+'__'+b]={
        'same_thread':x['body']['threadId']==y['body']['threadId'],
        'same_session':xh['x-session-id']==yh['x-session-id'],
        'same_project':xh['x-project-slug']==yh['x-project-slug'],
        'same_body':x['body']==y['body'],'fresh_trace':xh['traceparent']!=yh['traceparent']}
    save()

try:
    for i in range(100):
        if proc.poll() is not None: raise RuntimeError('proxy exited')
        try:
            with urllib.request.urlopen(url+'/healthz',timeout=1): break
        except Exception: time.sleep(.05)
    basic={'model':'deepseek/deepseek-v4-flash','max_tokens':args.max_tokens,'reasoning_effort':'low',
           'messages':[{'role':'system','content':'Reply with exactly OK. No explanation.'},{'role':'user','content':'Reply OK.'}]}
    run('missing_auth',basic,False)
    first=run('plain',basic)
    if not first.get('skipped') and not first.get('success'): raise RuntimeError('basic inference failed; stopping charged probes')
    run('repeat',basic);compare('plain','repeat')
    stream=copy.deepcopy(basic);stream['stream']=True;run('stream',stream);compare('plain','stream')
    extended=copy.deepcopy(basic);extended['messages'] += [{'role':'assistant','content':'OK'},{'role':'user','content':'Reply OK again.'}]
    run('extended',extended);compare('plain','extended')
    changed=copy.deepcopy(basic);changed['messages'][-1]['content']='A new conversation: reply OK.'
    run('different_root',changed);compare('plain','different_root')
    if args.restart_check:
        proc.send_signal(signal.SIGTERM);proc.wait(timeout=5)
        proc=subprocess.Popen([args.binary],cwd=ROOT,env=env,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        for i in range(100):
            try:
                with urllib.request.urlopen(url+'/healthz',timeout=1): break
            except Exception: time.sleep(.05)
        run('restart_same_request',basic);compare('plain','restart_same_request')
    if args.suite=='full':
        no_system=copy.deepcopy(basic);no_system['messages']=no_system['messages'][1:];run('no_system',no_system)
        blank=copy.deepcopy(basic);blank['messages'][0]['content']=' ';run('blank_system',blank)
        empty=copy.deepcopy(basic);empty['messages'][0]['content']='';run('empty_system',empty)
        developer=copy.deepcopy(basic);developer['messages'][0]['role']='developer';run('developer',developer)
        long_system='Reply with exactly OK. These are inert test labels. '+ ' '.join(f'Label {i}: use clear names and short functions.' for i in range(120))
        cache=copy.deepcopy(basic);cache['messages'][0]['content']=[{'type':'text','text':long_system,'cache_control':{'type':'ephemeral'}}]
        string_cache=copy.deepcopy(cache);string_cache['messages'][0]['content']=long_system
        run('cache_string',string_cache);run('cache_string_repeat',string_cache);compare('cache_string','cache_string_repeat')
        run('cache_blocks',cache);run('cache_repeat',cache);compare('cache_blocks','cache_repeat')
        off=copy.deepcopy(cache);off['prompt_cache']='off';run('cache_off',off);compare('cache_blocks','cache_off')
        tool=copy.deepcopy(basic);tool['max_tokens']=128;tool['messages']=[{'role':'system','content':'Call echo with value OK.'},{'role':'user','content':'Use the echo tool now.'}]
        tool['tools']=[{'type':'function','function':{'name':'echo','description':'Echo a test value','parameters':{'type':'object','properties':{'value':{'type':'string','enum':['OK']}},'required':['value'],'additionalProperties':False}}}]
        tool['tool_choice']='required'
        stream_tool=copy.deepcopy(tool);stream_tool['stream']=True;run('tool_call_stream',stream_tool)
        called=run('tool_call',tool)
        if called.get('tool_calls'):
            tc=called['tool_calls'][0];continued=copy.deepcopy(tool);continued['tool_choice']='none'
            continued['messages'] += [{'role':'assistant','content':None,'tool_calls':[tc]},{'role':'tool','tool_call_id':tc['id'],'content':'OK'},{'role':'user','content':'Reply OK. Do not call more tools.'}]
            run('tool_result_without_name',continued);compare('tool_call','tool_result_without_name')
        def chunk(kind,data): return struct.pack('>I',len(data))+kind+data+struct.pack('>I',zlib.crc32(kind+data))
        png=b'\x89PNG\r\n\x1a\n'+chunk(b'IHDR',struct.pack('>2I5B',16,16,8,2,0,0,0))+chunk(b'IDAT',zlib.compress((b'\0'+b'\xff\0\0'*16)*16))+chunk(b'IEND',b'')
        vision=copy.deepcopy(basic);vision['model']='deepseek/deepseek-v4-flash-vision-exp';vision['max_tokens']=max(128,args.max_tokens)
        vision['messages']=[{'role':'system','content':'Name the dominant image color using one word.'},{'role':'user','content':[{'type':'text','text':'What color is this square?'},{'type':'image_url','image_url':{'url':'data:image/png;base64,'+base64.b64encode(png).decode()}}]}]
        run('vision_data_url',vision)
        premium=copy.deepcopy(basic);premium['model']='gpt-6-astra';premium['max_tokens']=32;run('astra_access',premium)
        other=copy.deepcopy(basic);other['model']='Qwen/Qwen3.8-Max-0902';run('qwen',other)
        invalid=copy.deepcopy(basic);invalid['model']='invalid/compatibility-probe-nonexistent';run('invalid_model',invalid)
        image=copy.deepcopy(basic);image['messages'][-1]['content']=[{'type':'image_url','image_url':{'url':'https://example.com/test.png'}}];run('remote_image_rejected',image)
finally:
    save();proc.send_signal(signal.SIGTERM)
    try:proc.wait(timeout=5)
    except subprocess.TimeoutExpired:proc.kill();proc.wait()
    relay.shutdown();relay.server_close()
