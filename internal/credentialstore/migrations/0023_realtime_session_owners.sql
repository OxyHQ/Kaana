-- oxy:deploy-phase=pre
-- Additive, ephemeral placement metadata. Existing images ignore this table.
-- A request id names an execution, not an Oxy account or application entity.
CREATE TABLE realtime_session_owners (
    request_id TEXT PRIMARY KEY CHECK (length(request_id) BETWEEN 1 AND 128),
    lane TEXT NOT NULL CHECK (lane IN ('public', 'internal')),
    owner_address INET NOT NULL CHECK (family(owner_address)=4 AND masklen(owner_address)=32),
    owner_generation TEXT NOT NULL CHECK (owner_generation ~ '^[a-f0-9]{32}$'),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX realtime_session_owners_expiry_idx ON realtime_session_owners (expires_at);
REVOKE ALL ON realtime_session_owners FROM PUBLIC;

CREATE FUNCTION kaana_claim_realtime_owner(p_request TEXT,p_lane TEXT,p_address INET,p_generation TEXT)
RETURNS BOOLEAN LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE changed INTEGER;
BEGIN
    -- Bounded cleanup for crashed owners; no running session data is stored.
    DELETE FROM public.realtime_session_owners WHERE request_id IN (
        SELECT request_id FROM public.realtime_session_owners WHERE expires_at<=clock_timestamp()
        ORDER BY expires_at LIMIT 1000 FOR UPDATE SKIP LOCKED
    );
    INSERT INTO public.realtime_session_owners(request_id,lane,owner_address,owner_generation,expires_at)
    VALUES(p_request,p_lane,p_address,p_generation,clock_timestamp()+INTERVAL '45 seconds')
    ON CONFLICT (request_id) DO NOTHING;
    GET DIAGNOSTICS changed=ROW_COUNT;
    RETURN changed=1;
END $$;
CREATE FUNCTION kaana_lookup_realtime_owner(p_request TEXT,p_lane TEXT)
RETURNS TABLE(request_id TEXT,lane TEXT,owner_address TEXT,owner_generation TEXT,expires_at TIMESTAMPTZ)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public AS $$
    SELECT r.request_id,r.lane,host(r.owner_address),r.owner_generation,r.expires_at
    FROM public.realtime_session_owners r
    WHERE r.request_id=p_request AND r.lane=p_lane AND r.expires_at>clock_timestamp()
$$;
CREATE FUNCTION kaana_renew_realtime_owner(p_request TEXT,p_lane TEXT,p_address INET,p_generation TEXT)
RETURNS BOOLEAN LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE changed INTEGER;
BEGIN
    UPDATE public.realtime_session_owners SET expires_at=clock_timestamp()+INTERVAL '45 seconds'
    WHERE request_id=p_request AND lane=p_lane AND owner_address=p_address
      AND owner_generation=p_generation AND expires_at>clock_timestamp();
    GET DIAGNOSTICS changed=ROW_COUNT;
    RETURN changed=1;
END $$;
CREATE FUNCTION kaana_release_realtime_owner(p_request TEXT,p_lane TEXT,p_address INET,p_generation TEXT)
RETURNS VOID LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
    DELETE FROM public.realtime_session_owners WHERE request_id=p_request AND lane=p_lane
      AND owner_address=p_address AND owner_generation=p_generation
$$;
REVOKE ALL ON FUNCTION kaana_claim_realtime_owner(TEXT,TEXT,INET,TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_lookup_realtime_owner(TEXT,TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_renew_realtime_owner(TEXT,TEXT,INET,TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION kaana_release_realtime_owner(TEXT,TEXT,INET,TEXT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION kaana_claim_realtime_owner(TEXT,TEXT,INET,TEXT) TO kaana_runtime;
GRANT EXECUTE ON FUNCTION kaana_lookup_realtime_owner(TEXT,TEXT) TO kaana_runtime;
GRANT EXECUTE ON FUNCTION kaana_renew_realtime_owner(TEXT,TEXT,INET,TEXT) TO kaana_runtime;
GRANT EXECUTE ON FUNCTION kaana_release_realtime_owner(TEXT,TEXT,INET,TEXT) TO kaana_runtime;
