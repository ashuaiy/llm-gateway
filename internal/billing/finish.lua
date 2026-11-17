local expected={'hash','hash','zset','zset'}
for i=1,4 do
  local kind=redis.call('TYPE',KEYS[i]).ok
  if kind~='none' and kind~=expected[i] then return -3 end
end
local state=redis.call('HGET',KEYS[2],'state')
if not state then return -2 end
if state=='settled' or state=='released' then
  if state==ARGV[1] and redis.call('HGET',KEYS[2],'settlement')==ARGV[3] then return 1 end
  return -1
end
if state~='reserved' and state~='unknown' then return -3 end
if state=='unknown' and ARGV[1]=='released' then return -3 end
local amount=tonumber(redis.call('HGET',KEYS[2],'amount'))
local held=tonumber(redis.call('HGET',KEYS[1],'held'))
local spent=tonumber(redis.call('HGET',KEYS[1],'spent'))
local actual=tonumber(ARGV[2]); local max=tonumber(ARGV[6])
if not amount or not held or not spent or held<amount or spent<0 or actual>max-spent then return -3 end
if actual>amount then
  redis.call('HSET',KEYS[2],'state','unknown','reported_settlement',ARGV[3],'unresolved_reason','actual_exceeds_reserved')
  return -4
end
redis.call('HSET',KEYS[1],'held',string.format('%.0f',held-amount),'spent',string.format('%.0f',spent+actual))
redis.call('HSET',KEYS[2],'state',ARGV[1],'settlement',ARGV[3])
local clock=redis.call('TIME');local now=clock[1]*1000+math.floor(clock[2]/1000)
redis.call('ZADD',KEYS[3],now,ARGV[5])
redis.call('ZREM',KEYS[4],ARGV[5])
return 1
