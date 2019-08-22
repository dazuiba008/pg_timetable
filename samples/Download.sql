DO $$
	-- An example for Download task.
DECLARE
	v_task_id bigint;
	v_chain_id bigint;
	v_chain_config_id bigint;
BEGIN
	
	-- Get the base task id
	SELECT task_id INTO v_task_id FROM timetable.base_task WHERE name = 'Download';
	
	-- Create the chain
	INSERT INTO timetable.task_chain(task_id)
	VALUES (v_task_id)
	RETURNING chain_id INTO v_chain_id;

	-- Create the chain execution configuration
	INSERT INTO timetable.chain_execution_config VALUES 
    	(
        DEFAULT, -- chain_execution_config, 
        v_chain_id, -- chain_id, 
        'Download a file', -- chain_name
        NULL, -- run_at_minute, 
        NULL, -- run_at_hour, 
        NULL, -- run_at_day, 
        NULL, -- run_at_month,
        NULL, -- run_at_day_of_week, 
        1, -- max_instances, 
        TRUE, -- live, 
        FALSE, -- self_destruct,
        FALSE, -- exclusive_execution, 
        NULL -- excluded_execution_configs
    )
    RETURNING  chain_execution_config INTO v_chain_config_id;

	-- Create the parameters for the chain configuration

		--"workersnum":   Workerrs nummber - If the requested number of workers is less than one, a worker will be created
		--                for every request. 
		-- "fileurls":    Provide urls from where you wanna download files, User can mention n number of 
		--                comma separated urls 
		--"destpath":     Destination path 

	INSERT INTO timetable.chain_execution_parameters (
		chain_execution_config,
		chain_id,
		order_id,
		value
	) VALUES (
		v_chain_config_id,
		v_chain_id, 
		1, 
		'{
			"workersnum":   1, 
			"fileurls":   ["http://www.golang-book.com/public/pdf/gobook.pdf"], 
			"destpath": "/Users/Lenovo/Downloads"
		}'::jsonb
		);

END;
$$
LANGUAGE 'plpgsql';
